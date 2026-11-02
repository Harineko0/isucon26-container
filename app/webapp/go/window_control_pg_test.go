package isureal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func TestOpenWindowWhileRequestPoolIsFull(t *testing.T) {
	control := loginTestDB(t) // Isolated local schema with users, posting windows, and the SQL functions.
	var schema string
	if err := control.Get(&schema, "SELECT current_schema()"); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("ISUREAL_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	shared, err := sqlx.Connect("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	shared.SetMaxOpenConns(1)
	held, err := shared.Connx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	control.MustExec(`INSERT INTO administrators(id,username,password_hash) VALUES ('adm','admin','unused');
		INSERT INTO administrator_sessions(id,administrator_id,token_hash)
		VALUES ('session','adm',encode(sha256('control-token'::bytea),'hex'))`)
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	registerAdminPostingWindow(e, control)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	open := func() *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/admin/posting-window",
			strings.NewReader(`{"region":"asia","localDate":"2026-12-01"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set(echo.HeaderAuthorization, "Bearer control-token")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	rec := open()
	var opened openPostingWindowResponse
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &opened) != nil || opened.ID == "" {
		t.Fatalf("open with shared pool held: %d %s", rec.Code, rec.Body.String())
	}
	// Read on the held connection, independently of the control transaction.
	var activeID string
	if err := held.GetContext(ctx, &activeID, "SELECT id FROM posting_windows WHERE region='asia' AND ended_at IS NULL"); err != nil || activeID != opened.ID {
		t.Fatalf("committed active window: %q %v", activeID, err)
	}
	if rec := open(); rec.Code != http.StatusConflict {
		t.Fatalf("conflicting date: %d %s", rec.Code, rec.Body.String())
	}
	control.MustExec("UPDATE administrator_sessions SET active=false WHERE id='session'")
	if rec := open(); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session: %d %s", rec.Code, rec.Body.String())
	}

	// Even with the shared pool held, the first independent read after 201 sees
	// the durable notification. Changing the audience afterwards cannot move it.
	var count int
	checkCount := func(query string, want int) {
		t.Helper()
		if err := held.GetContext(ctx, &count, query, opened.ID); err != nil || count != want {
			t.Fatalf("count = %d, want %d (%v)", count, want, err)
		}
	}
	checkCount("SELECT count(*) FROM notifications WHERE posting_window_id=$1", 0)
	checkCount("SELECT count(*) FROM isureal_notifications WHERE posting_window_id=$1 AND user_id='u1'", 1)
	control.MustExec(`UPDATE users SET region='europe' WHERE id='u1';
	    INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES ('late','late',repeat('ab',32),'late','asia')`)
	checkCount("SELECT count(*) FROM isureal_notifications WHERE posting_window_id=$1 AND user_id='u1'", 1)
	checkCount("SELECT count(*) FROM isureal_notifications WHERE posting_window_id=$1 AND user_id='late'", 0)
	events, err := (windowDB{control}).notifications(ctx, "asia", opened.ID, []string{"late", "u1", "u2"})
	if err != nil || len(events) != 2 || events[0].UserID != "u1" || !events[0].kick || events[1].UserID != "u2" || events[1].kick {
		t.Fatalf("window audience after move and registration: %+v (%v)", events, err)
	}
}
