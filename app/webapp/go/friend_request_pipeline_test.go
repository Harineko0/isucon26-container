package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestFriendRequestPipelineOnPostgres(t *testing.T) {
	db := loginTestDB(t)
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}, IPBans: newIPBanList()}
	queue := &writeQueue[relationshipResult]{db: db, sql: friendRequestSQL}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.POST("/api/friend-requests", createFriendRequestHandlerWithQueue(h, queue),
		SessionMiddleware(h, true), RegionMiddleware(h))

	for _, id := range []string{"u1", "u2", "u3"} {
		digest := sha256.Sum256([]byte("pipeline-" + id))
		tokenHash := hex.EncodeToString(digest[:])
		db.MustExec(`INSERT INTO login_sessions(id,user_id,token_hash) VALUES($1,$1,$2)`, id, tokenHash)
		knownSessions.Store(tokenHash, Session{UserID: id, TokenHash: tokenHash, Region: "asia"})
		t.Cleanup(func() { knownSessions.Delete(tokenHash) })
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	held, err := db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	waitBefore := db.Stats().WaitCount

	canceled, abort := context.WithCancel(ctx)
	defer abort()
	cases := []struct {
		actor, target string
		ctx           context.Context
		want          int
		code          string
	}{
		{"u1", "u10", ctx, 201, ""},
		{"u1", "u10", ctx, 409, "relationship_conflict"},
		{"u2", "u11", ctx, 401, "invalid_session"},
		{"u3", "u12", ctx, 403, "region_mismatch"},
		{"u1", "missing", ctx, 404, "relationship_not_found"},
		{"u1", "u15", canceled, 0, ""},
		{"u1", "u16", ctx, 201, ""},
	}
	replies := make([]chan *httptest.ResponseRecorder, len(cases))
	for i, c := range cases {
		reply := make(chan *httptest.ResponseRecorder, 1)
		replies[i] = reply
		go func() {
			req := httptest.NewRequestWithContext(c.ctx, http.MethodPost, "/api/friend-requests",
				strings.NewReader(`{"recipientId":"`+c.target+`"}`))
			req.Header.Set(echo.HeaderAuthorization, "Bearer pipeline-"+c.actor)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			reply <- rec
		}()
		for {
			queue.mu.Lock()
			queued := queue.pending != nil && len(queue.pending.calls) == i+1
			queue.mu.Unlock()
			if queued && db.Stats().WaitCount > waitBefore {
				break
			}
			if ctx.Err() != nil {
				t.Fatalf("request %d did not join the pending pipeline", i)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// Authorization may change while the entire group is waiting for the only
	// connection. The mutations must use the committed values on checkout.
	if _, err := held.ExecContext(ctx, `UPDATE login_sessions SET active=false WHERE user_id='u2'; UPDATE users SET region='europe' WHERE id='u3'`); err != nil {
		t.Fatal(err)
	}
	abort()
	select {
	case <-replies[5]:
	case <-ctx.Done():
		t.Fatal("cancelled caller stayed queued")
	}
	if waits := db.Stats().WaitCount - waitBefore; waits != 1 {
		t.Fatalf("7 overlapping requests should share one checkout, got %d", waits)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		if c.want == 0 {
			continue
		}
		var rec *httptest.ResponseRecorder
		select {
		case rec = <-replies[i]:
		case <-ctx.Done():
			t.Fatalf("request %d did not complete", i)
		}
		if rec.Code != c.want {
			t.Fatalf("request %d: %d %s, want %d", i, rec.Code, rec.Body, c.want)
		}
		if c.code != "" {
			var body APIErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != c.code {
				t.Fatalf("request %d: %s, want %s", i, rec.Body, c.code)
			}
			continue
		}
		var row createFriendRequestResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil || row.Status != "pending" || row.UpdatedAt.IsZero() {
			t.Fatalf("request %d returned invalid row: %s", i, rec.Body)
		}
		var stored string
		if err := db.Get(&stored, `SELECT recipient_id FROM friend_requests WHERE id=$1 AND requester_id=$2`, row.RelationID, c.actor); err != nil || stored != c.target {
			t.Fatalf("request %d returned uncommitted or incorrect row: target=%s err=%v", i, stored, err)
		}
	}
	var rows, transactions int
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT xmin::text) FROM friend_requests`).Scan(&rows, &transactions); err != nil || rows != 2 || transactions != 2 {
		t.Fatalf("expected two independently committed rows; rows=%d transactions=%d err=%v", rows, transactions, err)
	}
}
