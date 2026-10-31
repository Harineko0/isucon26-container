package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestRegisterPipelineOnPostgres(t *testing.T) {
	db := loginTestDB(t)
	db.MustExec(`CREATE UNIQUE INDEX users_username_key ON users(username);
		ALTER TABLE users ADD CHECK (username <> 'force-failure')`)
	queue := &writeQueue[sessionUserRow]{db: db, sql: registerSQL}
	indexed := make(chan User, 4)
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.POST("/api/auth/register", registerHandlerWithQueue(queue, func(u User) { indexed <- u }))
	e.POST("/api/auth/login", loginHandler(&HandlerContext{DB: db, IPBans: newIPBanList()}))

	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	held, err := db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	before := db.Stats().WaitCount
	cases := []struct {
		name, display string
		status        int
	}{
		{"pipeline-a", "最初", http.StatusCreated},
		{"pipeline-a", "重複", http.StatusConflict},
		{"force-failure", "DB error", http.StatusInternalServerError},
		{"pipeline-b", "最後", http.StatusCreated},
	}
	replies := make([]chan *httptest.ResponseRecorder, len(cases))
	for i, c := range cases {
		reply := make(chan *httptest.ResponseRecorder, 1)
		replies[i] = reply
		go func() {
			body := fmt.Sprintf(`{"username":%q,"displayName":%q,"region":"europe"}`, c.name, c.display)
			reply <- serveRoundTrip(e, "", http.MethodPost, "/api/auth/register", body)
		}()
		waitForQueuedWrites(t, ctx, queue, i+1, before)
	}
	if waits := db.Stats().WaitCount - before; waits != 1 {
		t.Fatalf("%d queued registrations made %d pool checkouts wait", len(cases), waits)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	createdIDs := make(map[string]string)
	for i, c := range cases {
		var rec *httptest.ResponseRecorder
		select {
		case rec = <-replies[i]:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if rec.Code != c.status {
			t.Fatalf("%s: HTTP %d, want %d", c.name, rec.Code, c.status)
		}
		if c.status == http.StatusConflict {
			var failure APIErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil || failure.Error != "username_taken" {
				t.Fatalf("duplicate registration returned %s", rec.Body)
			}
		}
		if c.status != http.StatusCreated {
			continue
		}
		var created registerResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.SecretKey == "" ||
			created.User.Username != c.name || created.User.DisplayName != c.display || created.User.Region != "europe" {
			t.Fatalf("incorrect registration response for %s", c.name)
		}
		// A successful HTTP response must expose a usable credential for this
		// caller, including after the preceding transaction rolled back.
		body, _ := json.Marshal(loginRequest{Username: c.name, SecretKey: created.SecretKey})
		login := serveRoundTrip(e, "", http.MethodPost, "/api/auth/login", string(body))
		var loggedIn loginResponse
		if login.Code != http.StatusOK || json.Unmarshal(login.Body.Bytes(), &loggedIn) != nil ||
			loggedIn.Token == "" || loggedIn.User != created.User {
			t.Fatalf("immediate login for %s: HTTP %d or incorrect user", c.name, login.Code)
		}
		tokenHash := sha256.Sum256([]byte(loggedIn.Token))
		knownSessions.Delete(hex.EncodeToString(tokenHash[:]))
		createdIDs[created.User.ID] = c.display
	}
	if len(indexed) != 2 {
		t.Fatalf("scheduled %d search updates for two successful registrations", len(indexed))
	}
	close(indexed)
	for u := range indexed {
		if createdIDs[u.ID] != u.DisplayName {
			t.Fatalf("unexpected search update for %s", u.Username)
		}
		delete(createdIDs, u.ID)
	}
	var rows, transactions int
	err = db.QueryRow(`SELECT count(*), count(DISTINCT xmin::text) FROM users
		WHERE username IN ('pipeline-a','force-failure','pipeline-b')`).Scan(&rows, &transactions)
	if err != nil || rows != 2 || transactions != 2 {
		t.Fatalf("rows=%d transactions=%d err=%v", rows, transactions, err)
	}
}
