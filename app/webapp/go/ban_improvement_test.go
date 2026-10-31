package isureal

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// 改善の効果 (#109) を確かめるテスト。どちらも「改善前」の動きと並べて、攻撃のリクエストが
// ハンドラ (= DB) に届く件数が減ることを数で確かめる。

// newBanTestServer は本番と同じ順にミドルウェアを積んだサーバー。ハンドラは攻撃に対する応答
// (ログイン失敗・不正な画像・偽造署名) を返し、呼ばれた回数を数える。
// inProcess が false なら action log を数えない (改善前: BAN は isureal-guard だけが約 1 秒後に書く)。
func newBanTestServer(inProcess bool) (*echo.Echo, *atomic.Int64) {
	bans := newIPBanList()
	bans.snapshot.Store(&ipBanSnapshot{bans: map[string]time.Time{}}) // ip_bans は空 (guard はまだ書いていない)
	h := &HandlerContext{IPBans: bans}

	var observe func(action string, status int, ip, abuse string)
	if inProcess {
		observe = bans.observeActionLog
	}
	e := echo.New()
	e.IPExtractor = extractClientIP
	e.HTTPErrorHandler = APIErrorHandler
	e.Use(ActionLogMiddleware("info", io.Discard, observe))
	e.Use(IPBanMiddleware(h))

	reached := &atomic.Int64{}
	e.POST("/api/auth/login", func(c *echo.Context) error {
		reached.Add(1)
		return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
	})
	e.PUT("/api/me/profile-image", func(c *echo.Context) error {
		reached.Add(1)
		c.Set("abuse", "invalid_image")
		return NewAPIError(http.StatusBadRequest, "invalid_image")
	})
	e.GET("/api/images/:imageId", func(c *echo.Context) error {
		reached.Add(1)
		c.Set("abuse", "invalid_signature")
		return NewAPIError(http.StatusForbidden, "image_forbidden")
	})
	return e, reached
}

func TestInProcessBanStopsSequentialAttackAtThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		path      string
		threshold int
	}{
		{name: "login failures", method: http.MethodPost, path: "/api/auth/login", threshold: 10},
		{name: "invalid profile images", method: http.MethodPut, path: "/api/me/profile-image", threshold: 2},
		{name: "forged image signatures", method: http.MethodGet, path: "/api/images/img1?exp=1&sig=x", threshold: 3},
	}
	const attempts = 30
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			send := func(e *echo.Echo, ip string) int {
				req := httptest.NewRequest(tt.method, tt.path, nil)
				req.Header.Set("X-Client-IP", ip)
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				return rec.Code
			}

			// 改善前: guard の BAN が DB に載るまで (約 1.1 秒) は、すべてハンドラまで届く。
			before, beforeReached := newBanTestServer(false)
			for i := 0; i < attempts; i++ {
				send(before, "10.0.0.1")
			}
			if got := beforeReached.Load(); got != attempts {
				t.Fatalf("without in-process ban: %d of %d reached the handler, want all", got, attempts)
			}

			// 改善後: 閾値ちょうどでハンドラに届かなくなり、以降は 403 ip_banned。
			after, afterReached := newBanTestServer(true)
			for i := 0; i < attempts; i++ {
				code := send(after, "10.0.0.1")
				if i >= tt.threshold && code != http.StatusForbidden {
					t.Fatalf("request %d after the threshold: status %d, want 403", i+1, code)
				}
			}
			if got := afterReached.Load(); got != int64(tt.threshold) {
				t.Fatalf("with in-process ban: %d of %d reached the handler, want %d", got, attempts, tt.threshold)
			}
			// 別の IP は BAN されない。
			send(after, "10.0.0.2")
			if got := afterReached.Load(); got != int64(tt.threshold)+1 {
				t.Fatal("a different IP was blocked")
			}
			t.Logf("%s: reached the handler %d -> %d of %d", tt.name, attempts, tt.threshold, attempts)
		})
	}
}

// 同時に送られた攻撃: 入口の判定を通ったあと接続待ちの間に BAN された分は、DB に流さない。
func TestConnUnlessBannedStopsBurstWaitingForConnection(t *testing.T) {
	t.Parallel()
	const burst = 20

	// run は、接続 1 本のプールでその 1 本を握ったまま burst 件を接続待ちにし、
	// 待っている間に IP を BAN してから接続を放す。DB で実行された件数を返す。
	run := func(t *testing.T, acquire func(context.Context, *sqlx.DB) (*sqlx.Conn, error), bans *IPBanList) int64 {
		t.Helper()
		raw, err := sql.Open("isureal-noop", "")
		if err != nil {
			t.Fatal(err)
		}
		db := sqlx.NewDb(raw, "pgx")
		defer db.Close()
		db.SetMaxOpenConns(1)

		held, err := db.Connx(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var executed atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < burst; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := acquire(context.Background(), db)
				if err != nil {
					return
				}
				executed.Add(1) // ここで isureal_login などを実行する
				conn.Close()
			}()
		}
		deadline := time.Now().Add(5 * time.Second)
		for db.Stats().WaitCount < burst {
			if time.Now().After(deadline) {
				t.Fatalf("only %d requests are waiting for a connection", db.Stats().WaitCount)
			}
			time.Sleep(time.Millisecond)
		}
		// 待っている間に閾値に達して BAN された。
		bans.abuse.observe("invalid_image", "10.0.0.1", time.Now())
		bans.abuse.observe("invalid_image", "10.0.0.1", time.Now())
		held.Close()
		wg.Wait()
		return executed.Load()
	}

	beforeBans := newIPBanList()
	before := run(t, func(ctx context.Context, db *sqlx.DB) (*sqlx.Conn, error) {
		return db.Connx(ctx) // 改善前: 入口でしか判定しない
	}, beforeBans)
	if before != burst {
		t.Fatalf("without the re-check: %d of %d ran, want all", before, burst)
	}

	afterBans := newIPBanList()
	after := run(t, func(ctx context.Context, db *sqlx.DB) (*sqlx.Conn, error) {
		return afterBans.connUnlessBanned(ctx, db, "10.0.0.1")
	}, afterBans)
	if after != 0 {
		t.Fatalf("with the re-check: %d of %d ran, want 0", after, burst)
	}
	t.Logf("burst of %d waiting for a connection: ran %d -> %d", burst, before, after)
}
