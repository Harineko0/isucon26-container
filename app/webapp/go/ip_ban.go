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
	watch     sync.Once
	snapshot  atomic.Pointer[ipBanSnapshot]
	abuse     *abuseDetector
}

func newIPBanList() *IPBanList {
	return &IPBanList{abuse: newAbuseDetector()}
}

type ipBanSnapshot struct {
	loadedAt time.Time // SELECT を始めた時刻。これより後の書き込みは入っていないかもしれない
	bans     map[string]time.Time
}

type ipBan struct {
	IP          string    `db:"ip"`
	BannedUntil time.Time `db:"banned_until"`
}

func (l *IPBanList) Banned(ctx context.Context, db *sqlx.DB, ip string) (bool, error) {
	if l.abuse != nil && l.abuse.bannedAt(ip, time.Now()) {
		return true, nil
	}
	snapshot := l.snapshot.Load()
	if snapshot == nil && l.reloading.TryLock() {
		// 初回だけ同期で読み、以降は裏で 200ms ごとに読み直す(リクエストに DB 往復を乗せない)。
		defer l.reloading.Unlock()
		if err := l.reloadNow(ctx, db); err != nil {
			return false, err
		}
		snapshot = l.snapshot.Load()
	}
	if snapshot == nil {
		return false, nil
	}
	until, ok := snapshot.bans[ip]
	return ok && time.Now().Before(until), nil
}

// bannedCached は DB を読まずに、アプリ内の BAN と読み込み済みの ip_bans だけで判定する。
func (l *IPBanList) bannedCached(ip string, now time.Time) bool {
	if l.abuse != nil && l.abuse.bannedAt(ip, now) {
		return true
	}
	snapshot := l.snapshot.Load()
	if snapshot == nil {
		return false
	}
	until, ok := snapshot.bans[ip]
	return ok && now.Before(until)
}

// connUnlessBanned は DB 接続を取ってから BAN をもう一度確かめる。攻撃は何十件も同時に送られ、
// 入口の判定を通ったあと接続を待つ間に BAN されることが多いので、その分を DB に流さない (#109)。
// BAN 中なら入口と同じ 403 ip_banned を返す。呼び出し側は conn.Close() で接続をプールに返す。
func (l *IPBanList) connUnlessBanned(ctx context.Context, db *sqlx.DB, ip string) (*sqlx.Conn, error) {
	conn, err := db.Connx(ctx)
	if err != nil {
		return nil, err
	}
	if l.bannedCached(ip, time.Now()) {
		conn.Close()
		return nil, NewAPIError(http.StatusForbidden, "ip_banned")
	}
	return conn, nil
}

func (l *IPBanList) reloadLoop(db *sqlx.DB) {
	ticker := time.NewTicker(ipBanReloadInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		loaded, err := loadIPBans(ctx, db)
		cancel()
		if err == nil {
			l.store(loaded)
		}
	}
}

// store は s を、今あるものより後に読み始めたときだけ置く。reloadLoop の古い結果が
// reloadNow の結果を上書きしないようにする。
func (l *IPBanList) store(s *ipBanSnapshot) bool {
	for {
		cur := l.snapshot.Load()
		if cur != nil && cur.loadedAt.After(s.loadedAt) {
			return false
		}
		if l.snapshot.CompareAndSwap(cur, s) {
			return true
		}
	}
}

// A peer reset can be this server's first request. Start polling after either
// this explicit refresh or the first public request loads a snapshot.
func (l *IPBanList) reloadNow(ctx context.Context, db *sqlx.DB) error {
	loaded, err := loadIPBans(ctx, db)
	if err != nil {
		return err
	}
	l.store(loaded)
	l.watch.Do(func() { go l.reloadLoop(db) })
	return nil
}

func loadIPBans(ctx context.Context, db *sqlx.DB) (*ipBanSnapshot, error) {
	startedAt := time.Now()
	var rows []ipBan
	if err := db.SelectContext(ctx, &rows, `SELECT ip, banned_until FROM ip_bans WHERE banned_until > clock_timestamp()`); err != nil {
		return nil, err
	}
	bans := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		bans[row.IP] = row.BannedUntil
	}
	return &ipBanSnapshot{loadedAt: startedAt, bans: bans}, nil
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
			banned, err := h.IPBans.Banned(c.Request().Context(), h.DB, c.RealIP())
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
