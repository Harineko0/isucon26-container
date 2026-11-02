package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// memoryTestDB supplies an isolated local schema with users u1..u70.
func loginTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db := memoryTestDB(t)
	for _, file := range []string{"../sql/007-image-location.sql", "../sql/004-functions.sql"} {
		script, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		db.MustExec(string(script))
	}
	return db
}

func TestLoginPipelineTransactions(t *testing.T) {
	db := loginTestDB(t)
	db.MustExec(`CREATE TABLE allowed_tokens(token text PRIMARY KEY);
	  INSERT INTO allowed_tokens VALUES ('first'), ('second'), ('third');
	  ALTER TABLE login_sessions ADD FOREIGN KEY(token_hash) REFERENCES allowed_tokens(token)
	    DEFERRABLE INITIALLY DEFERRED;`)

	key1, key2 := strings.Repeat("0", 63)+"1", strings.Repeat("0", 63)+"2"
	inputs := []struct {
		args   []any
		expect string // user ID on success, invalid credentials, or SQLSTATE on failure
	}{
		{[]any{"user1", key1, "session1", "first"}, "u1"},
		{[]any{"user2", key2, "session1", "second"}, "23505"},    // statement fails
		{[]any{"user1", key1, "rollback", "forbidden"}, "23503"}, // commit fails
		{[]any{"user1", key1, "bad-arg", make(chan int)}, "encoding error"},
		{[]any{"user1", key2, "invalid", "unused"}, "invalid"},
		{[]any{"user2", key2, "session2", "second"}, "u2"},
		{[]any{"user1", key1, "session3", "third"}, "u1"},
	}
	var calls []*writeCall[sessionUserRow]
	for _, input := range inputs {
		calls = append(calls, &writeCall[sessionUserRow]{args: input.args, reply: make(chan writeResult[sessionUserRow], 1)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	q := writeQueue[sessionUserRow]{sql: loginSQL}
	q.pipeline(ctx, conn, calls)
	for i, call := range calls {
		result := <-call.reply
		var pgErr *pgconn.PgError
		got := result.row.Result
		switch {
		case errors.As(result.err, &pgErr):
			got = pgErr.Code
		case result.err != nil && strings.Contains(result.err.Error(), "failed to encode args[3]"):
			got = "encoding error"
		case result.err != nil:
			t.Fatalf("login %d: %v", i, result.err)
		case got == "ok":
			got = *result.row.ID
		}
		if got != inputs[i].expect {
			t.Fatalf("login %d: got %s, want %s", i, got, inputs[i].expect)
		}
	}
	var state string
	if err := db.Get(&state, `SELECT string_agg(token_hash || '=' || coalesce(reason, 'active'), ',' ORDER BY id)
	  FROM login_sessions`); err != nil || state != "first=replaced,second=active,third=active" {
		t.Fatalf("committed sessions: %q, err=%v", state, err)
	}
}

func TestLoginQueueRechecksBanAndCancellation(t *testing.T) {
	db := loginTestDB(t)
	db.SetMaxOpenConns(1)
	conn, err := db.Connx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	bans := newIPBanList()
	q := &writeQueue[sessionUserRow]{db: db, bans: bans, sql: loginSQL}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waiting, cancelWaiting := context.WithCancel(ctx)
	defer cancelWaiting()
	key := strings.Repeat("0", 63) + "1"
	invoke := func(ctx context.Context, ip string, count int) <-chan error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			_, err := q.execute(ctx, ip, "user1", key, "s"+ip, "t"+ip)
			done <- err
		}()
		for {
			q.mu.Lock()
			joined := q.pending != nil && len(q.pending.calls) == count
			q.mu.Unlock()
			if joined {
				return done
			}
			if ctx.Err() != nil {
				t.Fatal("login failed to join pending batch")
			}
			time.Sleep(time.Millisecond)
		}
	}
	ok := invoke(ctx, "ok", 1)
	banned := invoke(ctx, "banned", 2)
	canceled := invoke(waiting, "canceled", 3)
	bans.abuse.local.Store("banned", time.Now().Add(time.Minute))
	cancelWaiting()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled login: %v", err)
	}
	conn.Close()
	if err := <-ok; err != nil {
		t.Fatal(err)
	}
	var apiErr *APIError
	if err := <-banned; !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || apiErr.Code != "ip_banned" {
		t.Fatalf("banned login: %v", err)
	}
	var ids []string
	if err := db.Select(&ids, "SELECT id FROM login_sessions"); err != nil || len(ids) != 1 || ids[0] != "sok" {
		t.Fatalf("sessions: %v, err=%v", ids, err)
	}
}

func TestConcurrentLoginTokens(t *testing.T) {
	db := loginTestDB(t)
	db.SetMaxOpenConns(1)
	hash := sha256.Sum256([]byte("login-test-key"))
	db.MustExec("UPDATE users SET secret_key_hash=$1 WHERE id='u1'", hex.EncodeToString(hash[:]))
	h := &HandlerContext{DB: db, IPBans: newIPBanList()}
	e := echo.New()
	e.POST("/api/auth/login", loginHandler(h))

	const requests = 16
	replies := make(chan *httptest.ResponseRecorder, requests)
	for range requests {
		go func() {
			body := strings.NewReader(`{"username":"user1","secretKey":"login-test-key"}`)
			req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			replies <- rec
		}()
	}
	tokens := make(map[string]bool)
	for range requests {
		rec := <-replies
		var result loginResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.User.ID != "u1" || result.Token == "" {
			t.Fatalf("invalid login response, status %d", rec.Code)
		}
		sum := sha256.Sum256([]byte(result.Token))
		tokenHash := hex.EncodeToString(sum[:])
		t.Cleanup(func() { knownSessions.Delete(tokenHash) })
		tokens[tokenHash] = true
	}
	var active []string
	err := db.Select(&active, "SELECT token_hash FROM login_sessions WHERE user_id='u1' AND active")
	if err != nil || len(tokens) != requests || len(active) != 1 || !tokens[active[0]] {
		t.Fatalf("issued %d distinct tokens, %d active, err=%v", len(tokens), len(active), err)
	}
}

func TestGroupedLoginsCommitOnce(t *testing.T) {
	db := loginTestDB(t)
	key1, key2 := strings.Repeat("0", 63)+"1", strings.Repeat("0", 63)+"2"
	queue := &writeQueue[sessionUserRow]{sql: loginSQL, accountBatchSQL: loginBatchSQL}
	conn, err := db.Connx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	calls := make([]*writeCall[sessionUserRow], 0, 3)
	for _, args := range [][]any{
		{"user2", key2, "first", "t1"}, {"user1", key1, "second", "t2"}, {"user2", key2, "third", "t3"},
	} {
		calls = append(calls, &writeCall[sessionUserRow]{ctx: t.Context(), args: args, reply: make(chan writeResult[sessionUserRow], 1)})
	}
	if !queue.batchAccounts(t.Context(), conn, calls) {
		t.Fatal("valid login batch fell back")
	}
	for i, call := range calls {
		r := <-call.reply
		if r.err != nil || r.row.Username == nil || *r.row.Username != call.args[0] {
			t.Fatalf("login %d: %+v", i, r)
		}
	}
	var txns, active int
	if err := db.QueryRow("SELECT COUNT(DISTINCT xmin::text), count(*) FILTER (WHERE active AND id IN ('second','third')) FROM login_sessions WHERE id IN ('first','second','third')").Scan(&txns, &active); err != nil || txns != 1 || active != 2 {
		t.Fatalf("txns=%d, active=%d, err=%v", txns, active, err)
	}
}

func TestGroupedLoginsDeferredConstraint(t *testing.T) {
	db := loginTestDB(t)
	db.MustExec(`CREATE TABLE allowed_tokens(token text PRIMARY KEY);
		INSERT INTO allowed_tokens VALUES ('accepted');
		ALTER TABLE login_sessions ADD FOREIGN KEY(token_hash) REFERENCES allowed_tokens(token)
			DEFERRABLE INITIALLY DEFERRED`)
	q := writeQueue[sessionUserRow]{sql: loginSQL, accountBatchSQL: loginBatchSQL}
	conn, err := db.Connx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	key := strings.Repeat("0", 63) + "1"
	var calls []*writeCall[sessionUserRow]
	for _, token := range []string{"accepted", "rejected"} {
		calls = append(calls, &writeCall[sessionUserRow]{ctx: t.Context(), args: []any{"user1", key, token, token}, reply: make(chan writeResult[sessionUserRow], 1)})
	}
	if q.batchAccounts(t.Context(), conn, calls) {
		t.Fatal("returned results from a failed commit")
	}
	var committed int
	if err := conn.GetContext(t.Context(), &committed, "SELECT count(*) FROM login_sessions WHERE id IN ('accepted','rejected')"); err != nil || committed != 0 {
		t.Fatalf("committed before replay: %d, err=%v", committed, err)
	}
	q.pipeline(t.Context(), conn, calls)
	if r := <-calls[0].reply; r.err != nil || r.row.Result != "ok" {
		t.Fatalf("valid login: %+v", r)
	}
	var constraint *pgconn.PgError
	if r := <-calls[1].reply; !errors.As(r.err, &constraint) || constraint.Code != "23503" {
		t.Fatalf("invalid login: %+v", r)
	}
}
