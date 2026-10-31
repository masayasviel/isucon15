package isureal

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type FriendRequest struct {
	ID          string    `db:"id"`
	RequesterID string    `db:"requester_id"`
	RecipientID string    `db:"recipient_id"`
	Status      string    `db:"status"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

const friendRequestColumns = `id, requester_id, recipient_id, status, created_at, updated_at`

func getFriendRequest(ctx context.Context, db sqlx.QueryerContext, id string) (FriendRequest, error) {
	var request FriendRequest
	err := sqlx.GetContext(ctx, db, &request, `SELECT `+friendRequestColumns+` FROM friend_requests WHERE id=$1`, id)
	return request, err
}

func updateFriendRequestStatus(ctx context.Context, tx *sqlx.Tx, id, status string) (FriendRequest, error) {
	var request FriendRequest
	err := tx.GetContext(ctx, &request, `UPDATE friend_requests SET status=$2, updated_at=clock_timestamp() WHERE id=$1 RETURNING `+friendRequestColumns, id, status)
	return request, err
}

// 本人は排他、相手は共有でロックする。同じ2人への操作は直列のまま、別々のユーザーから同じ相手への操作は競合しない。
// FOR KEY SHARE は FOR NO KEY UPDATE と競合せず、2人が互いを本人として行う操作が同時に通るため使わない。
func beginRelationshipTx(ctx context.Context, db *sqlx.DB, actor, target string) (*sqlx.Tx, error) {
	if actor == target || target == "" {
		return nil, NewAPIError(http.StatusBadRequest, "invalid_relationship")
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, id := range []string{min(actor, target), max(actor, target)} {
		lockMode := "FOR SHARE"
		if id == actor {
			lockMode = "FOR NO KEY UPDATE"
		}
		var lockedID string
		if err := tx.GetContext(ctx, &lockedID, `SELECT id FROM users WHERE id=$1 AND status='active' `+lockMode, id); err != nil {
			tx.Rollback()
			if errors.Is(err, sql.ErrNoRows) {
				return nil, NewAPIError(http.StatusNotFound, "relationship_not_found")
			}
			return nil, err
		}
	}
	return tx, nil
}

func insertFriendship(ctx context.Context, tx *sqlx.Tx, request FriendRequest) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id) VALUES ($1,$2,$3,$4)`, uuid.NewString(), min(request.RequesterID, request.RecipientID), max(request.RequesterID, request.RecipientID), request.ID)
	return err
}

// 申請の承認と友達関係の作成を1文で行う。
// user_low_id と user_high_id は insertFriendship と同じく Go の比較で決める。SQL の LEAST / GREATEST は照合順序で結果が変わる。
func acceptFriendRequest(ctx context.Context, tx *sqlx.Tx, request FriendRequest) (FriendRequest, error) {
	var accepted FriendRequest
	err := tx.GetContext(ctx, &accepted, `
  WITH accepted AS (
    UPDATE friend_requests SET status='accepted', updated_at=clock_timestamp() WHERE id=$1
    RETURNING `+friendRequestColumns+`
  ), friendship AS (
    INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id) VALUES ($2,$3,$4,$1)
  )
  SELECT `+friendRequestColumns+` FROM accepted
 `, request.ID, uuid.NewString(), min(request.RequesterID, request.RecipientID), max(request.RequesterID, request.RecipientID))
	return accepted, err
}

func requireRelationshipChange(result sql.Result, err error, status int, code string) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return NewAPIError(status, code)
	}
	return nil
}
