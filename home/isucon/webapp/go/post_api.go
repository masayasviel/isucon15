package isureal

import (
	"bytes"
	"database/sql"
	"errors"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createPostRequest struct {
	Image        *multipart.FileHeader `form:"image"`
	Caption      string                `form:"caption"`
	Visibility   string                `form:"visibility"`
	LocationKind string                `form:"location_kind"`
	Latitude     *float64              `form:"latitude"`
	Longitude    *float64              `form:"longitude"`
	Locality     *string               `form:"locality"`
}

type postAuthorResponse struct {
	ID              string  `json:"id"`
	Username        string  `json:"username"`
	DisplayName     string  `json:"displayName"`
	ProfileImageURL *string `json:"profileImageUrl"`
	Bio             string  `json:"bio"`
	AccountType     string  `json:"accountType"`
}

type createPostResponse struct {
	ID              string             `json:"id"`
	Author          postAuthorResponse `json:"author"`
	PostingWindowID string             `json:"postingWindowId"`
	ImageURL        string             `json:"imageUrl"`
	Caption         string             `json:"caption"`
	Visibility      string             `json:"visibility"`
	Location        PostLocation       `json:"location"`
	PostedAt        time.Time          `json:"postedAt"`
	OnTime          bool               `json:"onTime"`
	LateSeconds     int64              `json:"lateSeconds"`
}

func createPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createPostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Image == nil || !validUserText(input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		switch input.Visibility {
		case "friends", "friends_of_friends", "public":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		location := PostLocation{
			Kind: input.LocationKind, Latitude: input.Latitude,
			Longitude: input.Longitude, Locality: input.Locality,
		}
		if !location.valid() {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		imageFile, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer imageFile.Close()
		imageData, err := io.ReadAll(imageFile)
		if err != nil {
			return err
		}
		img, err := jpeg.Decode(bytes.NewReader(imageData))
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if img.Bounds().Dx() > 4096 || img.Bounds().Dy() > 4096 {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}

		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		user, err := lockUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		// FOR SHARE は同じ SELECT の集約と併用できないため、投稿数はスカラーサブクエリで取得する。
		// 投稿の作成・削除はユーザー行のロックで直列化済みなので、投稿枠のロック待ち前のスナップショットで読んでも結果は変わらない。
		const windowQuery = `
   SELECT w.id, w.starts_at, w.on_time_deadline,
          (SELECT count(*) FROM posts p JOIN posting_windows pw ON pw.id = p.posting_window_id
           WHERE p.author_id = $2 AND pw.local_date = w.local_date AND p.deleted_at IS NULL) AS active,
          (SELECT count(*) FROM posts p JOIN posting_windows pw ON pw.id = p.posting_window_id
           WHERE p.author_id = $2 AND pw.local_date = w.local_date AND p.deleted_at IS NOT NULL) AS deleted
   FROM posting_windows w
   WHERE w.region = $1 AND w.ended_at IS NULL
   FOR SHARE OF w
  `
		var window struct {
			ID             string    `db:"id"`
			StartsAt       time.Time `db:"starts_at"`
			OnTimeDeadline time.Time `db:"on_time_deadline"`
			Active         int       `db:"active"`
			Deleted        int       `db:"deleted"`
		}
		err = tx.GetContext(ctx, &window, windowQuery, user.Region, userID)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.GetContext(ctx, &window, windowQuery, user.Region, userID)
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return NewAPIError(http.StatusConflict, "post_slot_unavailable")
			}
			return err
		}
		postedAt := time.Now().UTC()
		if window.Active != 0 || window.Deleted >= 2 || postedAt.Before(window.StartsAt) {
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		}
		imageID := uuid.NewString()
		postID := uuid.NewString()
		var post Post
		if err := tx.GetContext(ctx, &post, `
   WITH image AS (
     INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at)
     VALUES ($1, $2, $3, 'image/jpeg', $4)
   )
   INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                      location_kind, latitude, longitude, locality, posted_at)
   VALUES ($5, $2, $6, $1, $7, $8, $9, $10, $11, $12, $4)
   RETURNING id, author_id, posting_window_id, image_id, caption, visibility,
             location_kind, latitude, longitude, locality, posted_at
  `, imageID, userID, imageData, postedAt, postID, window.ID, input.Caption, input.Visibility,
			location.Kind, location.Latitude, location.Longitude, location.Locality); err != nil {
			return err
		}
		post.OnTimeDeadline = window.OnTimeDeadline
		if err := tx.Commit(); err != nil {
			return err
		}
		_ = writeImageFile(imageID, imageData)
		return c.JSON(http.StatusCreated, newPostResponse(post, user))
	}
}

func newPostResponse(post Post, user User) createPostResponse {
	response := createPostResponse{
		ID: post.ID,
		Author: postAuthorResponse{
			ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
			ProfileImageURL: newGetMeResponse(user).ProfileImageURL,
			Bio:             user.Bio, AccountType: user.AccountType,
		},
		PostingWindowID: post.PostingWindowID, ImageURL: signedImageURL(user, post.ImageID),
		Caption: post.Caption, Visibility: post.Visibility, Location: post.PostLocation,
		PostedAt: post.PostedAt, OnTime: !post.PostedAt.After(post.OnTimeDeadline),
	}
	if !response.OnTime {
		response.LateSeconds = int64(post.PostedAt.Sub(post.OnTimeDeadline) / time.Second)
	}
	return response
}

type getMyCurrentPostResponse createPostResponse

func getMyCurrentPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		posts, err := selectPostsWithAuthor(c.Request().Context(), h.DB, `
   AND p.author_id = $1
   AND w.local_date = (SELECT current.local_date FROM posting_windows current
                       WHERE current.region = u.region AND current.ended_at IS NULL)
  `, GetSession(c).UserID)
		if err != nil {
			return err
		}
		if len(posts) == 0 {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		return c.JSON(http.StatusOK, getMyCurrentPostResponse(newPostResponse(posts[0].Post, posts[0].Author)))
	}
}

type getPostRequest struct {
	PostID string `param:"postId"`
}

type getPostResponse createPostResponse

func getPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getPostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		post, err := getPostWithAuthor(ctx, h.DB, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		viewer, err := loadViewerContext(ctx, h.DB, GetSession(c).UserID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err != nil || !viewer.canView(post.Post) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		return c.JSON(http.StatusOK, getPostResponse(newPostResponse(post.Post, post.Author)))
	}
}

type updatePostRequest struct {
	PostID     string  `param:"postId" json:"-"`
	Caption    *string `json:"caption"`
	Visibility *string `json:"visibility"`
}

type updatePostResponse createPostResponse

func updatePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input updatePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Caption == nil && input.Visibility == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Caption != nil && !validUserText(*input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Visibility != nil {
			switch *input.Visibility {
			case "friends", "friends_of_friends", "public":
			default:
				return NewAPIError(http.StatusBadRequest, "invalid_post")
			}
		}
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		user, err := lockUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		post, err := getPost(ctx, tx, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if post.AuthorID != userID {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		if post.WindowEndedAt != nil {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE posts SET caption = coalesce($2, caption), visibility = coalesce($3, visibility) WHERE id = $1`, post.ID, input.Caption, input.Visibility); err != nil {
			return err
		}
		if input.Caption != nil {
			post.Caption = *input.Caption
		}
		if input.Visibility != nil {
			post.Visibility = *input.Visibility
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, updatePostResponse(newPostResponse(post, user)))
	}
}

type deletePostRequest struct {
	PostID string `param:"postId" json:"-"`
}

func deletePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deletePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var lockedUserID string
		if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID); err != nil {
			return err
		}
		post, err := getPost(ctx, tx, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if post.AuthorID != userID {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		window, err := getPostingWindow(ctx, tx, post.PostingWindowID)
		if err != nil {
			return err
		}
		var deletions int
		if err := tx.GetContext(ctx, &deletions, `
   SELECT count(*) FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND w.local_date = $2 AND p.deleted_at IS NOT NULL
  `, userID, window.LocalDate); err != nil {
			return err
		}
		if deletions >= 2 {
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE posts SET deleted_at = clock_timestamp() WHERE id = $1`, post.ID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
