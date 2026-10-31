package isureal

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

const ipBanReloadInterval = 200 * time.Millisecond

type IPBanList struct {
	reloading sync.Mutex
	snapshot  atomic.Pointer[ipBanSnapshot]
}

type ipBanSnapshot struct {
	loadedAt time.Time
	bans     map[string]time.Time
}

type ipBan struct {
	IP          string    `db:"ip"`
	BannedUntil time.Time `db:"banned_until"`
}

func (l *IPBanList) Banned(ctx context.Context, db *sqlx.DB, ip string) (bool, error) {
	snapshot := l.snapshot.Load()
	if snapshot == nil && l.reloading.TryLock() {
		// 初回だけ同期で読み、以降は裏で 200ms ごとに読み直す(リクエストに DB 往復を乗せない)。
		// db は専用プール (BgDB) で、リクエスト用プールの取り出し待ちに並ばない (#44)。
		defer l.reloading.Unlock()
		loaded, err := loadIPBans(ctx, db)
		if err != nil {
			return false, err
		}
		l.snapshot.Store(loaded)
		snapshot = loaded
		go l.reloadLoop(db)
	}
	if snapshot == nil {
		return false, nil
	}
	until, ok := snapshot.bans[ip]
	return ok && time.Now().Before(until), nil
}

func (l *IPBanList) reloadLoop(db *sqlx.DB) {
	ticker := time.NewTicker(ipBanReloadInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		loaded, err := loadIPBans(ctx, db)
		cancel()
		if err == nil {
			l.snapshot.Store(loaded)
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
	return &ipBanSnapshot{loadedAt: time.Now(), bans: bans}, nil
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
			banned, err := h.IPBans.Banned(c.Request().Context(), h.BgDB, c.RealIP())
			if err != nil {
				return err
			}
			if banned {
				return NewAPIError(http.StatusForbidden, "ip_banned")
			}
			return next(c)
		}
	}
}
