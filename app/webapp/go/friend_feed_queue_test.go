package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func waitForFriendFeedQueue(t *testing.T, ctx context.Context, check func() bool) {
	t.Helper()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for friend feed queue")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestFriendFeedQueueSeparatesRunningReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	var queries atomic.Int32
	rec := &recordingConnector{respond: func(query string) ([]string, [][]driver.Value) {
		if query != feedDeferredQueries["friends"] {
			t.Error("expected one live request to use the regular query")
		}
		id := "new"
		if queries.Add(1) == 1 {
			close(firstStarted)
			select {
			case <-firstRelease:
			case <-ctx.Done():
			}
			id = "old"
		}
		return []string{"id"}, [][]driver.Value{{id}}
	}}
	db := sqlx.NewDb(sql.OpenDB(rec), "pgx")
	defer db.Close()
	db.SetMaxOpenConns(1)
	held, err := db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	var q friendFeedQueue
	type result struct {
		rows []feedRow
		err  error
	}
	submit := func(ctx context.Context) <-chan result {
		done := make(chan result, 1)
		go func() {
			rows, err := q.selectRows(ctx, db, "asia", friendFeedRequest{userID: "viewer", tokenHash: "token"})
			done <- result{rows, err}
		}()
		return done
	}
	waitPending := func(size int) {
		waitForFriendFeedQueue(t, ctx, func() bool {
			q.mu.Lock()
			defer q.mu.Unlock()
			return q.pending != nil && len(q.pending.calls) == size
		})
	}

	waiting, cancelWaiting := context.WithCancel(ctx)
	first := submit(waiting)
	waitPending(1)
	live := submit(ctx)
	waitPending(2)
	cancelWaiting()
	if got := <-first; !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled waiter returned %v", got.err)
	}
	held.Close()
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("canceled first waiter affected live request")
	}
	late := submit(ctx)
	waitPending(1)
	close(firstRelease)
	for _, tc := range []struct {
		result <-chan result
		want   string
	}{{live, "old"}, {late, "new"}} {
		got := <-tc.result
		if got.err != nil || len(got.rows) != 1 || got.rows[0].ID != tc.want {
			t.Fatalf("want %q, got %+v, err=%v", tc.want, got.rows, got.err)
		}
	}
}

func TestFriendFeedQueueBoundsAndCancelsPoolWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	db, _ := openSlowExecDB(t, 0, 1)
	held, err := db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	var q friendFeedQueue
	group, cancelGroup := context.WithCancel(ctx)
	defer cancelGroup()
	done := make(chan error, maxFriendFeedBatch+1)
	for range maxFriendFeedBatch + 1 {
		go func() {
			_, err := q.selectRows(group, db, "asia", friendFeedRequest{userID: "viewer", tokenHash: "token"})
			done <- err
		}()
	}
	var last *friendFeedBatch
	waitForFriendFeedQueue(t, ctx, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		last = q.pending
		return last != nil && len(last.calls) == 1 && db.Stats().WaitCount == 2
	})
	cancelGroup()
	for range maxFriendFeedBatch + 1 {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	}
	select {
	case <-last.done:
	case <-ctx.Done():
		t.Fatal("empty queue still waiting for a connection")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending != nil {
		t.Fatal("canceled queue still admits requests")
	}
}
