package isureal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// Observe successful LISTENs without relying on a sleep to hit the reconnect gap.
type listenTrace chan struct{}

func (ch listenTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(q.SQL, "LISTEN ") {
		return context.WithValue(ctx, ch, true)
	}
	return ctx
}

func (ch listenTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryEndData) {
	if ctx.Value(ch) != nil && q.Err == nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// app/webapp/sql/tests/roundtrips.sh sets up a disposable PG16 database.
func TestRoundTripHandlersOnPostgresWindowNotifications(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set")
	}
	db, err := sqlx.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`TRUNCATE users, login_sessions, posting_windows CASCADE`,
		`INSERT INTO users(id,username,secret_key_hash,display_name,region)
		 VALUES ('sse-user','viewer',repeat('ab',32),'Viewer','europe')`,
		`INSERT INTO login_sessions(id,user_id,token_hash)
		 VALUES ('sse-session','sse-user',encode(sha256('window-token'::bytea),'hex'))`,
		`INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline)
		 VALUES ('initial','europe','2026-10-01',now(),now()), ('other','asia','2026-10-01',now(),now())`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["application_name"] = "test-window-watcher"
	listening := make(listenTrace, 2)
	cfg.Tracer = listening
	listenerDB := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	defer listenerDB.Close()
	listenerDB.SetMaxOpenConns(1) // delivery must reuse the LISTEN connection
	w := newPostingWindowWatcherWith("europe", nil)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.runWindowLoop(ctx, listenerDB)
	}()
	defer func() { stop(); <-done }()

	waitForListen := func() {
		t.Helper()
		select {
		case <-listening:
		case <-time.After(5 * time.Second):
			t.Fatal("listener did not subscribe")
		}
	}
	waitForListen()

	h := &HandlerContext{DB: db, Config: &Config{Region: "europe"}, Windows: w}
	e := echo.New()
	e.POST("/api/admin/posting-window", openPostingWindowHandler(h.DB, h.DB))
	e.GET("/api/notifications", getNotificationsHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	server := httptest.NewServer(e)
	defer server.Close()
	subscribe := func() *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, server.URL+"/api/notifications", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer window-token")
		resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
			resp.Body.Close()
			t.Fatalf("subscribe: %s, %s", resp.Status, resp.Header.Get("Content-Type"))
		}
		return resp
	}
	readNotification := func(resp *http.Response, id string) {
		t.Helper()
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		var n getNotificationsResponse
		line, ok := strings.CutPrefix(string(body), "data: ")
		if !ok || json.Unmarshal([]byte(line), &n) != nil {
			t.Fatalf("invalid SSE event: %q", body)
		}
		var want getNotificationsResponse
		if err := db.Get(&want, `SELECT isureal_notification_id(id, 'sse-user', starts_at) AS id,
			id AS posting_window_id, region, starts_at AS created_at FROM posting_windows WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if n.PostingWindowID != want.PostingWindowID || n.ID != want.ID || n.Region != want.Region || !n.CreatedAt.Equal(want.CreatedAt) {
			t.Fatalf("got %+v, want %+v", n, want)
		}
	}

	// Independently observe the writer so that periodic reconciliation cannot mask
	// a missing NOTIFY. A rolled-back hint must never reach either listener.
	observer, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close(ctx)
	channel := windowChannelPrefix + "europe"
	if _, err := observer.Exec(ctx, "LISTEN "+channel); err != nil {
		t.Fatal(err)
	}
	resp := subscribe()
	defer resp.Body.Close()
	if _, err := db.Exec(`BEGIN; SELECT pg_notify('isureal_window_europe','rolled-back'); ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT pg_notify($1, 'other')`, channel); err != nil {
		t.Fatal(err)
	}
	rec := serveRoundTrip(e, "", http.MethodPost, "/api/admin/posting-window", `{"region":"europe","localDate":"2026-10-02"}`)
	var opened openPostingWindowResponse
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &opened) != nil || opened.ID == "" {
		t.Fatalf("open window: %d %s", rec.Code, rec.Body.String())
	}
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, want := range []string{"other", opened.ID} {
		n, err := observer.WaitForNotification(waitCtx)
		if err != nil || n.Payload != want {
			t.Fatalf("DB notification: got %v (%v), want %s", n, err, want)
		}
	}
	readNotification(resp, opened.ID)

	// Reconnect with the same client. Duplicates, a closed period and an id from
	// another region must not escape. Lose the listener, then change the active
	// window without a signal (like initialize or a writer from an older build).
	resp = subscribe()
	defer resp.Body.Close()
	for _, id := range []string{opened.ID, "initial", "other"} {
		if _, err := db.Exec(`SELECT pg_notify($1, $2)`, channel, id); err != nil {
			t.Fatal(err)
		}
	}
	var pid int
	if err := db.Get(&pid, `SELECT pid FROM pg_stat_activity WHERE application_name = $1`, cfg.RuntimeParams["application_name"]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT pg_terminate_backend($1, 3000)`, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE posting_windows SET ended_at=now() WHERE region='europe' AND ended_at IS NULL;
		INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline)
		VALUES ('recovered','europe','2026-10-03',now(),now())`); err != nil {
		t.Fatal(err)
	}
	readNotification(resp, "recovered")
	waitForListen()
}
