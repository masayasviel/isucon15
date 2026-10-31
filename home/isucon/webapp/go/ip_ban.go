package isureal

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

const ipBanReloadInterval = 200 * time.Millisecond

type IPBanList struct {
	snapshot atomic.Pointer[ipBanSnapshot]
}

type ipBanSnapshot struct {
	bans map[string]time.Time
}

type ipBan struct {
	IP          string    `db:"ip"`
	BannedUntil time.Time `db:"banned_until"`
}

// 判定はスナップショットを見るだけで、DB への読み込みはリクエストの外の watch が行う。
func (l *IPBanList) Banned(ip string) bool {
	snapshot := l.snapshot.Load()
	if snapshot == nil {
		return false
	}
	until, ok := snapshot.bans[ip]
	return ok && time.Now().Before(until)
}

func (l *IPBanList) reload(ctx context.Context, db *sqlx.DB) error {
	loaded, err := loadIPBans(ctx, db)
	if err != nil {
		return err
	}
	l.snapshot.Store(loaded)
	return nil
}

// 読み込みに失敗したら直前のスナップショットを保ち、次の周期で読み直す。
func (l *IPBanList) watch(db *sqlx.DB) {
	ctx := context.Background()
	for {
		time.Sleep(ipBanReloadInterval)
		if err := l.reload(ctx, db); err != nil {
			slog.Error("failed to reload ip bans", "error", err)
		}
	}
}

func loadIPBans(ctx context.Context, db *sqlx.DB) (*ipBanSnapshot, error) {
	var rows []ipBan
	if err := db.SelectContext(ctx, &rows, `SELECT ip, banned_until FROM ip_bans WHERE banned_until > clock_timestamp()`); err != nil {
		return nil, err
	}
	bans := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		bans[row.IP] = row.BannedUntil
	}
	return &ipBanSnapshot{bans: bans}, nil
}

func extractClientIP(req *http.Request) string {
	if ip := req.Header.Get("X-Client-IP"); ip != "" {
		return ip
	}
	return echo.ExtractIPDirect()(req)
}

func IPBanMiddleware(h *HandlerContext) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if h.IPBans.Banned(c.RealIP()) {
				return NewAPIError(http.StatusForbidden, "ip_banned")
			}
			return next(c)
		}
	}
}
