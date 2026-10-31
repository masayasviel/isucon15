package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type getNotificationsResponse struct {
	ID              string    `json:"id" db:"id"`
	PostingWindowID string    `json:"postingWindowId" db:"posting_window_id"`
	Region          string    `json:"region" db:"region"`
	CreatedAt       time.Time `json:"createdAt" db:"created_at"`
}

type notificationResult struct {
	notification *getNotificationsResponse
	err          error
}

type notificationSubscription struct {
	userID      string
	connectedAt time.Time
	result      chan notificationResult
}

type notificationHub struct {
	mu            sync.Mutex
	subscriptions map[*notificationSubscription]struct{}
}

func (hub *notificationHub) subscribe(userID string, connectedAt time.Time) *notificationSubscription {
	subscription := &notificationSubscription{
		userID:      userID,
		connectedAt: connectedAt,
		// 受信側が切断しても監視 goroutine を止めない。結果は接続ごとに1回だけ送る。
		result: make(chan notificationResult, 1),
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.subscriptions == nil {
		hub.subscriptions = make(map[*notificationSubscription]struct{})
	}
	hub.subscriptions[subscription] = struct{}{}
	return subscription
}

func (hub *notificationHub) unsubscribe(subscription *notificationSubscription) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	delete(hub.subscriptions, subscription)
}

func (hub *notificationHub) snapshot() []*notificationSubscription {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	subscriptions := make([]*notificationSubscription, 0, len(hub.subscriptions))
	for subscription := range hub.subscriptions {
		subscriptions = append(subscriptions, subscription)
	}
	return subscriptions
}

func (hub *notificationHub) finish(subscription *notificationSubscription, result notificationResult) {
	hub.mu.Lock()
	_, subscribed := hub.subscriptions[subscription]
	delete(hub.subscriptions, subscription)
	hub.mu.Unlock()
	if subscribed {
		subscription.result <- result
	}
}

func watchNotifications(h *HandlerContext) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		h.notifications.poll(context.Background(), h.DB, h.Config.Region)
	}
}

func (hub *notificationHub) poll(ctx context.Context, db *sqlx.DB, region string) {
	// この周期の対象を固定し、クエリ中の新規接続を結果に含めない。
	// DB アクセス中も登録・解除を止めない。
	subscriptions := hub.snapshot()
	if len(subscriptions) == 0 {
		return
	}
	userIDs := make([]string, 0, len(subscriptions))
	seen := make(map[string]struct{}, len(subscriptions))
	for _, subscription := range subscriptions {
		if _, ok := seen[subscription.userID]; !ok {
			seen[subscription.userID] = struct{}{}
			userIDs = append(userIDs, subscription.userID)
		}
	}
	type recipient struct {
		UserID          string         `db:"user_id"`
		PostingWindowID sql.NullString `db:"posting_window_id"`
		Region          string         `db:"region"`
		StartsAt        sql.NullTime   `db:"starts_at"`
	}
	var recipients []recipient
	err := db.SelectContext(ctx, &recipients, `
  SELECT u.id AS user_id, w.id AS posting_window_id, u.region, w.starts_at
  FROM users u
  LEFT JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
  WHERE u.id = ANY($2::text[]) AND u.status = 'active' AND u.region = $1
 `, region, userIDs)
	if err != nil {
		for _, subscription := range subscriptions {
			hub.finish(subscription, notificationResult{err: err})
		}
		return
	}
	byUserID := make(map[string]recipient, len(recipients))
	for _, recipient := range recipients {
		byUserID[recipient.UserID] = recipient
	}
	for _, subscription := range subscriptions {
		recipient, ok := byUserID[subscription.userID]
		if !ok {
			// 退会・リージョン変更の確認を、通知の配信より優先する。
			hub.finish(subscription, notificationResult{})
			continue
		}
		if !recipient.PostingWindowID.Valid || !recipient.StartsAt.Valid || !recipient.StartsAt.Time.After(subscription.connectedAt) {
			continue
		}
		hub.finish(subscription, notificationResult{notification: &getNotificationsResponse{
			ID:              uuid.NewSHA1(uuid.NameSpaceOID, []byte(recipient.PostingWindowID.String+":"+subscription.userID)).String(),
			PostingWindowID: recipient.PostingWindowID.String,
			Region:          recipient.Region,
			CreatedAt:       recipient.StartsAt.Time,
		}})
	}
}

func getNotificationsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		session := GetSession(c)
		userID := session.UserID
		var connectedAt time.Time
		if err := h.DB.GetContext(ctx, &connectedAt, `SELECT clock_timestamp()`); err != nil {
			return err
		}
		response := c.Response()
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(response)
		if err := controller.Flush(); err != nil {
			return nil
		}
		subscription := h.notifications.subscribe(userID, connectedAt)
		defer h.notifications.unsubscribe(subscription)
		select {
		case <-ctx.Done():
			return nil
		case result := <-subscription.result:
			if result.err != nil {
				if ctx.Err() == nil {
					c.Logger().Error("failed to get notification", "error", result.err)
				}
				return nil
			}
			if result.notification == nil {
				return nil
			}
			data, err := json.Marshal(result.notification)
			if err != nil {
				c.Logger().Error("failed to encode notification", "error", err)
				return nil
			}
			if _, err := fmt.Fprintf(response, "data: %s\n\n", data); err != nil {
				return nil
			}
			_ = controller.Flush()
			return nil
		}
	}
}
