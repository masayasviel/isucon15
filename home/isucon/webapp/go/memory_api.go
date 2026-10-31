package isureal

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type getMemoriesRequest struct {
	Month          string     `query:"month"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type memoryPostResponse struct {
	createPostResponse
	RealMojis []replaceRealMojiResponse `json:"realmojis"`
	Comments  []createCommentResponse   `json:"comments"`
}

type getMemoriesResponse struct {
	Items   []memoryPostResponse `json:"items"`
	HasMore bool                 `json:"hasMore"`
}

type getMemoriesByDateRequest struct {
	Date           string     `param:"date"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type getMemoriesByDateResponse getMemoriesResponse

func getMemoriesHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse("2006-01", input.Month)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 1, 0), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, response)
	}
}

func getMemoriesByDateHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesByDateRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse(time.DateOnly, input.Date)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 0, 1), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMemoriesByDateResponse(response))
	}
}

// memoryPostRow は Memories 用の1投稿分の行。RealMoji とコメントは SQL 側で投稿ごとに
// 最大50件に絞り、json_agg で1カラムにまとめて受け取る（DB 往復を1回にするため）。
type memoryPostRow struct {
	PostWithAuthor
	RealMojisJSON string `db:"realmojis_json"`
	CommentsJSON  string `db:"comments_json"`
}

type memoryRealMojiJSON struct {
	ID           string    `json:"id"`
	ImageID      string    `json:"image_id"`
	ReactionType string    `json:"reaction_type"`
	CreatedAt    time.Time `json:"created_at"`
	Reactor      userJSON  `json:"reactor"`
}

type memoryCommentJSON struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	Author    userJSON  `json:"author"`
}

const memoryInteractionLimit = 50

func listMemories(ctx context.Context, db sqlx.QueryerContext, userID string, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, error) {
	var response getMemoriesResponse
	if (beforeAt == nil) != (beforeID == "") {
		return response, NewAPIError(http.StatusBadRequest, "invalid_pagination")
	}
	var rows []memoryPostRow
	if err := sqlx.SelectContext(ctx, db, &rows, `
  WITH memory_posts AS (
    SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
    FROM posts p
    JOIN posting_windows w ON w.id = p.posting_window_id
    JOIN users u ON u.id = p.author_id AND u.status = 'active'
    WHERE p.deleted_at IS NULL
      AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
      AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
    ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
  )
  SELECT mp.*,
    (SELECT coalesce(json_agg(r ORDER BY r.created_at DESC, r.id DESC), '[]')::text
     FROM (
       SELECT r.id, r.image_id, r.reaction_type, r.created_at, `+userJSONObject("u")+` AS reactor
       FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
       WHERE r.post_id = mp.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
       ORDER BY r.created_at DESC, r.id DESC LIMIT $6
     ) r) AS realmojis_json,
    (SELECT coalesce(json_agg(c ORDER BY c.created_at, c.id), '[]')::text
     FROM (
       SELECT c.id, c.body, c.created_at, `+userJSONObject("u")+` AS author
       FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
       WHERE c.post_id = mp.id AND c.deleted_at IS NULL
       ORDER BY c.created_at, c.id LIMIT $6
     ) c) AS comments_json
  FROM memory_posts mp
  ORDER BY mp.posted_at DESC, mp.id DESC
 `, userID, start, end, beforeAt, beforeID, memoryInteractionLimit); err != nil {
		return response, err
	}
	response.HasMore = len(rows) > 50
	if response.HasMore {
		rows = rows[:50]
	}
	items := make([]memoryPostResponse, 0, len(rows))
	for _, row := range rows {
		if err := row.Author.setImageKey(); err != nil {
			return response, err
		}
		item := memoryPostResponse{
			createPostResponse: newPostResponse(row.Post, row.Author),
			RealMojis:          make([]replaceRealMojiResponse, 0),
			Comments:           make([]createCommentResponse, 0),
		}
		var reactions []memoryRealMojiJSON
		if err := json.Unmarshal([]byte(row.RealMojisJSON), &reactions); err != nil {
			return response, err
		}
		for _, reaction := range reactions {
			reactor, err := reaction.Reactor.user()
			if err != nil {
				return response, err
			}
			item.RealMojis = append(item.RealMojis, newRealMojiResponse(RealMoji{
				ID: reaction.ID, PostID: row.ID, ReactorID: reactor.ID, ImageID: reaction.ImageID,
				ReactionType: reaction.ReactionType, CreatedAt: reaction.CreatedAt.In(time.Local),
			}, reactor))
		}
		var comments []memoryCommentJSON
		if err := json.Unmarshal([]byte(row.CommentsJSON), &comments); err != nil {
			return response, err
		}
		for _, comment := range comments {
			author, err := comment.Author.user()
			if err != nil {
				return response, err
			}
			item.Comments = append(item.Comments, newCommentResponse(Comment{
				ID: comment.ID, PostID: row.ID, AuthorID: author.ID, Body: comment.Body,
				CreatedAt: comment.CreatedAt.In(time.Local),
			}, author))
		}
		items = append(items, item)
	}
	response.Items = items
	return response, nil
}
