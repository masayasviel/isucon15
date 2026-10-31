package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// 実 DB を使わず、1周期の問い合わせ回数と受信者ごとの配信を確認する。
type notificationTestQuery func([]driver.NamedValue) (driver.Rows, error)

type notificationTestConnector struct {
	query notificationTestQuery
}

func (c notificationTestConnector) Connect(context.Context) (driver.Conn, error) {
	return notificationTestConn{query: c.query}, nil
}

func (c notificationTestConnector) Driver() driver.Driver {
	return notificationTestDriver{}
}

type notificationTestDriver struct{}

func (notificationTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use notificationTestConnector")
}

type notificationTestConn struct {
	query notificationTestQuery
}

func (notificationTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}

func (notificationTestConn) Close() error { return nil }

func (notificationTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

func (notificationTestConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c notificationTestConn) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	return c.query(args)
}

type notificationTestRows struct {
	values [][]driver.Value
}

func (*notificationTestRows) Columns() []string {
	return []string{"user_id", "posting_window_id", "region", "starts_at"}
}

func (*notificationTestRows) Close() error { return nil }

func (r *notificationTestRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}

func notificationTestDB(t *testing.T, query notificationTestQuery) *sqlx.DB {
	t.Helper()
	db := sqlx.NewDb(sql.OpenDB(notificationTestConnector{query: query}), "pgx")
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func notificationTestResult(t *testing.T, subscription *notificationSubscription) notificationResult {
	t.Helper()
	select {
	case result := <-subscription.result:
		return result
	default:
		t.Fatal("connection has no result")
		return notificationResult{}
	}
}

func notificationTestWaiting(t *testing.T, subscription *notificationSubscription) {
	t.Helper()
	select {
	case result := <-subscription.result:
		t.Fatalf("connection ended unexpectedly: %+v", result)
	default:
	}
}

func TestNotificationPollNoConnections(t *testing.T) {
	var hub notificationHub
	// nil DB でも問い合わせを行わず return する。
	hub.poll(context.Background(), nil, "asia")
	subscription := hub.subscribe("user", time.Now())
	hub.unsubscribe(subscription)
	hub.poll(context.Background(), nil, "asia")
}

func TestNotificationPollSharedDelivery(t *testing.T) {
	startsAt := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	var hub notificationHub
	first := hub.subscribe("first", startsAt.Add(-time.Second))
	sameUser := hub.subscribe("first", startsAt.Add(-time.Millisecond))
	second := hub.subscribe("second", startsAt.Add(-time.Second))
	deleted := hub.subscribe("deleted", startsAt.Add(-time.Second))
	moved := hub.subscribe("moved", startsAt.Add(-time.Second))
	equal := hub.subscribe("first", startsAt)
	after := hub.subscribe("second", startsAt.Add(time.Second))
	queries := 0
	db := notificationTestDB(t, func(args []driver.NamedValue) (driver.Rows, error) {
		queries++
		if len(args) != 2 || args[0].Value != "asia" {
			t.Fatalf("unexpected arguments: %v", args)
		}
		ids, ok := args[1].Value.([]string)
		if !ok || len(ids) != 4 {
			t.Fatalf("expected four distinct user IDs: %v", args[1].Value)
		}
		seen := make(map[string]bool)
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("duplicate user ID: %s", id)
			}
			seen[id] = true
		}
		return &notificationTestRows{values: [][]driver.Value{
			{"first", "window", "asia", startsAt},
			{"second", "window", "asia", startsAt},
		}}, nil
	})
	hub.poll(context.Background(), db, "asia")
	if queries != 1 {
		t.Fatalf("got %d queries, want 1", queries)
	}
	firstResult := notificationTestResult(t, first)
	sameResult := notificationTestResult(t, sameUser)
	secondResult := notificationTestResult(t, second)
	for _, result := range []notificationResult{firstResult, sameResult, secondResult} {
		if result.err != nil || result.notification == nil {
			t.Fatalf("expected notification, got %+v", result)
		}
		n := result.notification
		if n.PostingWindowID != "window" || n.Region != "asia" || !n.CreatedAt.Equal(startsAt) || n.ID == "" {
			t.Fatalf("unexpected notification: %+v", n)
		}
	}
	if firstResult.notification.ID != sameResult.notification.ID {
		t.Fatal("same user and window produced different notification IDs")
	}
	if firstResult.notification.ID == secondResult.notification.ID {
		t.Fatal("different users produced the same notification ID")
	}
	for _, subscription := range []*notificationSubscription{deleted, moved} {
		result := notificationTestResult(t, subscription)
		if result.err != nil || result.notification != nil {
			t.Fatalf("invalid recipient received notification: %+v", result)
		}
	}
	notificationTestWaiting(t, equal)
	notificationTestWaiting(t, after)
	if len(hub.snapshot()) != 2 {
		t.Fatal("completed connections remain subscribed")
	}
	hub.unsubscribe(equal)
	hub.unsubscribe(after)
	hub.poll(context.Background(), db, "asia")
	if queries != 1 {
		t.Fatal("query issued after all connections ended")
	}
}

func TestNotificationPollWithoutOpenWindow(t *testing.T) {
	var hub notificationHub
	subscription := hub.subscribe("user", time.Now())
	db := notificationTestDB(t, func([]driver.NamedValue) (driver.Rows, error) {
		return &notificationTestRows{values: [][]driver.Value{{"user", nil, "asia", nil}}}, nil
	})
	hub.poll(context.Background(), db, "asia")
	notificationTestWaiting(t, subscription)
}

func TestNotificationPollDatabaseError(t *testing.T) {
	var hub notificationHub
	first := hub.subscribe("first", time.Now())
	second := hub.subscribe("second", time.Now())
	wantErr := errors.New("database unavailable")
	db := notificationTestDB(t, func([]driver.NamedValue) (driver.Rows, error) {
		return nil, wantErr
	})
	hub.poll(context.Background(), db, "asia")
	for _, subscription := range []*notificationSubscription{first, second} {
		result := notificationTestResult(t, subscription)
		if !errors.Is(result.err, wantErr) || result.notification != nil {
			t.Fatalf("expected database error, got %+v", result)
		}
	}
	if len(hub.snapshot()) != 0 {
		t.Fatal("connections remain subscribed after database error")
	}
}

func TestNotificationPollConnectionChangesDuringQuery(t *testing.T) {
	var hub notificationHub
	startsAt := time.Now()
	canceled := hub.subscribe("canceled", startsAt.Add(-time.Second))
	var added *notificationSubscription
	db := notificationTestDB(t, func([]driver.NamedValue) (driver.Rows, error) {
		// クエリ中も切断・登録でき、新規接続にはこの周期の結果を適用しない。
		hub.unsubscribe(canceled)
		added = hub.subscribe("added", startsAt.Add(-time.Second))
		return &notificationTestRows{values: [][]driver.Value{{"canceled", "window", "asia", startsAt}}}, nil
	})
	hub.poll(context.Background(), db, "asia")
	notificationTestWaiting(t, canceled)
	notificationTestWaiting(t, added)
	if subscriptions := hub.snapshot(); len(subscriptions) != 1 || subscriptions[0] != added {
		t.Fatal("new connection was removed by an earlier query result")
	}
}
