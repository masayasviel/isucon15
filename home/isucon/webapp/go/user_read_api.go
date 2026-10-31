package isureal

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type searchUsersRequest struct {
	Q              string `query:"q"`
	BeforeUsername string `query:"beforeUsername"`
	BeforeID       string `query:"beforeId"`
}

type searchUsersResponse struct {
	Items   []relationshipUserResponse `json:"items"`
	HasMore bool                       `json:"hasMore"`
}

func normalizeSearchText(value string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
}

// users.display_name_search に normalizeSearchText(display_name) を保持し、検索の部分一致を SQL 側で行う。
// 登録と表示名の変更で書き込むほか、初期データや reset.sql で戻された表示名にも追従できるよう、
// initialize と起動時に全行を突き合わせて値が違う行だけ更新する。
func syncDisplayNameSearch(ctx context.Context, db *sqlx.DB) error {
	var rows []struct {
		ID          string  `db:"id"`
		DisplayName string  `db:"display_name"`
		Search      *string `db:"display_name_search"`
	}
	if err := db.SelectContext(ctx, &rows, `SELECT id, display_name, display_name_search FROM users`); err != nil {
		return err
	}
	ids := make([]string, 0)
	searches := make([]string, 0)
	for _, row := range rows {
		search := normalizeSearchText(row.DisplayName)
		if row.Search != nil && *row.Search == search {
			continue
		}
		ids = append(ids, row.ID)
		searches = append(searches, search)
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `
  UPDATE users u SET display_name_search = v.search
  FROM unnest($1::text[], $2::text[]) AS v(id, search)
  WHERE u.id = v.id AND u.display_name_search IS DISTINCT FROM v.search
 `, ids, searches)
	return err
}

func searchUsersHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input searchUsersRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Q, 1, 100) {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		if (input.BeforeUsername == "") != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		// 部分一致・ブロック除外・ページ送りを SQL 側で行い、DB から転送する行を最大51件にする。
		// username は [a-z0-9_.-] のみなので normalizeSearchText しても値は変わらず、そのまま比較できる。
		var users []User
		if err := h.DB.SelectContext(ctx, &users, `
  WITH blocked AS (
    SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END AS id
    FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
  )
  SELECT `+strings.Join(userColumns, ", ")+` FROM users u
  WHERE u.status = 'active' AND u.id <> $1
    AND (strpos(u.username, $4) > 0 OR strpos(u.display_name_search, $4) > 0)
    AND NOT EXISTS (SELECT 1 FROM blocked WHERE blocked.id = u.id)
    AND ($2 = '' OR (u.username, u.id) > ($2, $3))
  ORDER BY u.username, u.id LIMIT 51
 `, viewer, input.BeforeUsername, input.BeforeID, normalizeSearchText(input.Q)); err != nil {
			return err
		}
		items := make([]relationshipUserResponse, 0)
		for _, user := range users {
			if err := user.setImageKey(); err != nil {
				return err
			}
			if user.AccountType != "certified_public" {
				user.Bio = ""
			}
			items = append(items, newRelationshipUserResponse(user))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, searchUsersResponse{Items: items, HasMore: hasMore})
	}
}

type getUserRequest struct {
	UserID string `param:"userId"`
}

type getUserResponse relationshipUserResponse

func getUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		user, err := getUser(ctx, h.DB, input.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		blocked, err := isBlocked(ctx, h.DB, viewer, user.ID)
		if err != nil {
			return err
		}
		if blocked {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		friends, err := getFriendIDs(ctx, h.DB, viewer)
		if err != nil {
			return err
		}
		if user.AccountType != "certified_public" && !slices.Contains(friends, user.ID) {
			user.Bio = ""
		}
		return c.JSON(http.StatusOK, getUserResponse(newRelationshipUserResponse(user)))
	}
}

func newRelationshipUserResponse(user User) relationshipUserResponse {
	return relationshipUserResponse{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, ProfileImageURL: newGetMeResponse(user).ProfileImageURL, Bio: user.Bio, AccountType: user.AccountType}
}
