package isureal

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jmoiron/sqlx"
)

// Hold one SELECT open and issue a later request for the same token. It must
// see the second snapshot. Cancelling one original waiter must not cancel its peers.
func TestWindowReadsDoNotReuseInflightSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type query struct {
			ctx    context.Context
			tokens []string
			reply  chan int
		}
		queries := make(chan query, 3)
		r := &windowReads{load: func(ctx context.Context, tokens []string) (map[string]currentPostingWindowState, error) {
			q := query{ctx, tokens, make(chan int)}
			queries <- q
			select {
			case n := <-q.reply:
				rows := make(map[string]currentPostingWindowState)
				for _, token := range tokens {
					rows[token] = currentPostingWindowState{Active: n}
				}
				return rows, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
		type result struct {
			n   int
			err error
		}
		get := func(ctx context.Context, token string) <-chan result {
			out := make(chan result, 1)
			go func() { v, _, err := r.get(ctx, token); out <- result{v.Active, err} }()
			return out
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		lost, first := get(ctx, "A"), get(t.Context(), "B")
		synctest.Wait()
		time.Sleep(2 * time.Millisecond)
		q1 := <-queries
		if len(q1.tokens) != 2 {
			t.Fatalf("first query has %d tokens", len(q1.tokens))
		}
		cancel()
		if got := <-lost; !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancelled: %+v", got)
		}
		if q1.ctx.Err() != nil {
			t.Fatal("one caller cancelled its peer's SELECT")
		}
		later := get(t.Context(), "B")
		synctest.Wait()
		time.Sleep(2 * time.Millisecond)
		q2 := <-queries
		q2.reply <- 2
		q1.reply <- 1
		if a, b := <-first, <-later; a != (result{1, nil}) || b != (result{2, nil}) {
			t.Fatalf("results: %+v, %+v", a, b)
		}
		ctx2, cancel2 := context.WithCancel(t.Context())
		last := get(ctx2, "C")
		synctest.Wait()
		time.Sleep(2 * time.Millisecond)
		q3 := <-queries
		cancel2()
		<-last
		synctest.Wait()
		if q3.ctx.Err() == nil {
			t.Fatal("query survived cancellation of all waiters")
		}
	})
}

// Test the production query against an isolated transaction on local PG16.
func TestWindowReadsCurrentState(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ISUREAL_TEST_DATABASE_URL to a local PostgreSQL database")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("test database must be local")
	}
	db, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTxx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tx.MustExec(`CREATE TEMP TABLE users(id text,region text,status text);
	CREATE TEMP TABLE login_sessions(token_hash text,user_id text,active boolean);
	CREATE TEMP TABLE posting_windows(id text,region text,local_date date,starts_at timestamptz,on_time_deadline timestamptz,ended_at timestamptz);
	CREATE TEMP TABLE posts(author_id text,posting_window_id text,deleted_at timestamptz);
	INSERT INTO users VALUES ('a','asia','active'),('b','asia','active'),('c','europe','active'),('d','asia','deleted');
	INSERT INTO login_sessions VALUES ('A','a',true),('B','b',true),('C','c',true),('D','d',true),('old','b',false);
	INSERT INTO posting_windows VALUES ('asia-now','asia','2026-10-31',now(),now()+interval '1 minute',NULL),
	('eu-closed','europe','2026-10-31',now(),now(),now()),('asia-old','asia','2026-10-30',now(),now(),now());
	INSERT INTO posts VALUES ('a','eu-closed',NULL),('a','asia-now',now()),('a','asia-old',NULL),('b','asia-now',now());`)
	tokens := []string{"A", "B", "C", "D", "old", "missing", "A"}
	fetch := func() map[string]currentPostingWindowState {
		rows, err := loadCurrentWindows(t.Context(), tx, "asia", tokens)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	rows := fetch()
	if len(rows) != 3 || rows["A"].ID == nil || *rows["A"].ID != "asia-now" || rows["A"].Active != 1 || rows["A"].Deleted != 1 || rows["B"].Active != 0 || rows["B"].Deleted != 1 || rows["C"].SessRegion != "europe" {
		t.Fatalf("unexpected current state: %+v", rows)
	}
	tx.MustExec(`UPDATE posts SET deleted_at=now() WHERE author_id='a' AND posting_window_id='eu-closed'; UPDATE login_sessions SET active=false WHERE token_hash='B'`)
	rows = fetch()
	if len(rows) != 2 || rows["A"].Active != 0 || rows["A"].Deleted != 2 {
		t.Fatal("stale deletion or session")
	}
	tx.MustExec(`UPDATE posting_windows SET ended_at=now() WHERE id='asia-now'`)
	rows = fetch()
	if rows["A"].ID != nil || rows["A"].Active != 0 || rows["A"].Deleted != 0 {
		t.Fatal("closed window still visible")
	}
}
