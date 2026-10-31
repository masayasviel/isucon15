package isureal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

const notificationPollInterval = 500 * time.Millisecond

type getNotificationsResponse struct {
	ID              string    `json:"id"`
	PostingWindowID string    `json:"postingWindowId"`
	Region          string    `json:"region"`
	CreatedAt       time.Time `json:"createdAt"`
}

// events には配信する通知を送る。配信せずに接続を閉じるときは nil を送る。
type notificationSubscriber struct {
	userID      string
	connectedAt time.Time
	events      chan *getNotificationsResponse
}

// 接続中の SSE の確認を、サーバごとに1本のクエリへまとめる。
type NotificationHub struct {
	mu          sync.Mutex
	subscribers map[*notificationSubscriber]struct{}
}

func newNotificationHub() *NotificationHub {
	return &NotificationHub{subscribers: map[*notificationSubscriber]struct{}{}}
}

func (hub *NotificationHub) subscribe(userID string, connectedAt time.Time) *notificationSubscriber {
	subscriber := &notificationSubscriber{userID: userID, connectedAt: connectedAt, events: make(chan *getNotificationsResponse, 1)}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.subscribers[subscriber] = struct{}{}
	return subscriber
}

func (hub *NotificationHub) unsubscribe(subscriber *notificationSubscriber) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	delete(hub.subscribers, subscriber)
}

func (hub *NotificationHub) currentSubscribers() []*notificationSubscriber {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	subscribers := make([]*notificationSubscriber, 0, len(hub.subscribers))
	for subscriber := range hub.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	return subscribers
}

// 購読を外してから送るので1接続に1回だけ送られ、容量1の events で待たされることはない。
func (hub *NotificationHub) finish(subscriber *notificationSubscriber, event *getNotificationsResponse) {
	hub.unsubscribe(subscriber)
	subscriber.events <- event
}

type notificationState struct {
	UserID   string     `db:"user_id"`
	WindowID *string    `db:"window_id"`
	StartsAt *time.Time `db:"starts_at"`
}

// 通知は投稿枠から導出する。行が返らないユーザーは退会したかリージョンを変更している。
func loadNotificationStates(ctx context.Context, db *sqlx.DB, region string, subscribers []*notificationSubscriber) (map[string]notificationState, error) {
	userIDs := make([]string, len(subscribers))
	for i, subscriber := range subscribers {
		userIDs[i] = subscriber.userID
	}
	var rows []notificationState
	if err := db.SelectContext(ctx, &rows, `
  SELECT u.id AS user_id, w.id AS window_id, w.starts_at
  FROM users u
  LEFT JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
  WHERE u.id = ANY($1::text[]) AND u.region = $2 AND u.status = 'active'
 `, userIDs, region); err != nil {
		return nil, err
	}
	states := make(map[string]notificationState, len(rows))
	for _, row := range rows {
		states[row.UserID] = row
	}
	return states, nil
}

// DB エラーのときは states が空になり、接続中の SSE をすべて閉じる。
func (hub *NotificationHub) watch(db *sqlx.DB, region string) {
	ctx := context.Background()
	ticker := time.NewTicker(notificationPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		subscribers := hub.currentSubscribers()
		if len(subscribers) == 0 {
			continue
		}
		states, err := loadNotificationStates(ctx, db, region, subscribers)
		if err != nil {
			slog.Error("failed to get notifications", "error", err)
		}
		for _, subscriber := range subscribers {
			state, ok := states[subscriber.userID]
			switch {
			case !ok:
				hub.finish(subscriber, nil)
			case state.WindowID != nil && state.StartsAt.After(subscriber.connectedAt):
				hub.finish(subscriber, &getNotificationsResponse{
					ID:              uuid.NewSHA1(uuid.NameSpaceOID, []byte(*state.WindowID+":"+subscriber.userID)).String(),
					PostingWindowID: *state.WindowID,
					Region:          region,
					CreatedAt:       *state.StartsAt,
				})
			}
		}
	}
}

func getNotificationsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
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
		subscriber := h.Notifications.subscribe(GetSession(c).UserID, connectedAt)
		defer h.Notifications.unsubscribe(subscriber)
		select {
		case <-ctx.Done():
			return nil
		case notification := <-subscriber.events:
			if notification == nil {
				return nil
			}
			data, err := json.Marshal(notification)
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
