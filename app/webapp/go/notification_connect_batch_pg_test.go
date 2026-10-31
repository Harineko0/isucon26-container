package isureal

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// Count the actual statements on the request pool. A test can hold the first
// result after PostgreSQL has evaluated it and commit a change on another pool.
type notificationConnectTrace struct {
	singles   atomic.Int32
	batches   atomic.Int32
	evaluated chan struct{}
	release   chan struct{}
}

func (s *notificationConnectTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	switch q.SQL {
	case notificationConnectSQL:
		s.singles.Add(1)
	case notificationConnectBatchSQL:
		if s.batches.Add(1) == 1 && s.evaluated != nil {
			return context.WithValue(ctx, s, true)
		}
	}
	return ctx
}

func (s *notificationConnectTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(s) != nil {
		close(s.evaluated)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	}
}

func notificationConnectFixture(t *testing.T, trace *notificationConnectTrace) (*sqlx.DB, *notificationConnectQueue) {
	t.Helper()
	writer := memoryTestDB(t)
	writer.MustExec(`INSERT INTO login_sessions(id,user_id,token_hash) VALUES
		('s1','u1','t1'),('s2','u2','t2'),('s3','u3','t3'),('s4','u4','t4'),('s5','u5','t5');
		UPDATE users SET region='europe' WHERE id='u3';
		INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline)
		VALUES ('open','asia','2026-10-31','2026-10-31 00:00Z','2026-10-31 01:00Z');`)
	cfg, err := pgx.ParseConfig(os.Getenv("ISUREAL_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := writer.Get(&schema, "SELECT current_schema()"); err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	cfg.Tracer = trace
	reader := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	reader.SetMaxOpenConns(1)
	t.Cleanup(func() { reader.Close() })
	return writer, &notificationConnectQueue{db: reader, region: "asia"}
}

type notificationConnectReply struct {
	state notificationConnectState
	err   error
}

func queueNotificationConnect(ctx context.Context, queue *notificationConnectQueue, token, user string) <-chan notificationConnectReply {
	reply := make(chan notificationConnectReply, 1)
	go func() {
		state, err := queue.get(ctx, token, user)
		reply <- notificationConnectReply{state, err}
	}()
	return reply
}

func waitNotificationConnectQueue(t *testing.T, queue *notificationConnectQueue, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		queue.mu.Lock()
		count := 0
		if queue.pending != nil {
			count = len(queue.pending.calls)
		}
		queue.mu.Unlock()
		if count == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %d queued calls, got %d", n, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNotificationConnectBurstOnPostgres(t *testing.T) {
	trace := &notificationConnectTrace{}
	writer, queue := notificationConnectFixture(t, trace)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()

	// Hold the single pool slot until all clients have queued.
	conn, err := queue.db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	wantUsers := []struct{ token, user, region string }{
		{"t1", "u1", "asia"}, {"t1", "u2", ""}, {"t3", "u3", "europe"}, {"t4", "u4", ""}, {"t5", "u5", ""}, {"missing", "u1", ""},
	}
	const total = maxNotificationConnectBatch + 8
	var replies []<-chan notificationConnectReply
	for i := 0; i < total; i++ {
		tc := wantUsers[i%len(wantUsers)]
		replies = append(replies, queueNotificationConnect(ctx, queue, tc.token, tc.user))
		waitNotificationConnectQueue(t, queue, (i+1)%maxNotificationConnectBatch)
	}
	waiting, cancelWaiting := context.WithCancel(ctx)
	canceled := queueNotificationConnect(waiting, queue, "t2", "u2")
	waitNotificationConnectQueue(t, queue, total%maxNotificationConnectBatch+1)
	cancelWaiting()
	if r := <-canceled; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("canceled client: got %v, want context.Canceled", r.err)
	}

	// Revoke after admission. u5 is deleted in the fixture. Both clients must
	// get the invalid-session state, including when their neighbors are valid.
	writer.MustExec("UPDATE login_sessions SET active=false WHERE id='s4'")
	conn.Close()
	wantStart := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for i, reply := range replies {
		r := <-reply
		tc := wantUsers[i%len(wantUsers)]
		if r.err != nil || r.state.Found != (tc.region != "") {
			t.Fatalf("client %d, token/user %s/%s: state=%+v err=%v", i, tc.token, tc.user, r.state, r.err)
		}
		if r.state.Found && (r.state.Region == nil || *r.state.Region != tc.region || r.state.ActiveWindowStartsAt == nil || !r.state.ActiveWindowStartsAt.Equal(wantStart)) {
			t.Fatalf("client %d, token/user %s/%s: state=%+v err=%v", i, tc.token, tc.user, r.state, r.err)
		}
	}
	if got := trace.batches.Load(); got != 2 || trace.singles.Load() != 0 {
		t.Fatalf("%d requests: batch SQL=%d singleton SQL=%d", total, got, trace.singles.Load())
	}
	t.Logf("%d session checks under contention used 2 SELECTs", total)
	if _, err := queue.get(ctx, "t1", "u1"); err != nil || trace.singles.Load() != 1 {
		t.Fatalf("idle-pool subscription must use original single SELECT: %v, count=%d", err, trace.singles.Load())
	}
}

func TestNotificationConnectLateSubscriberOnPostgres(t *testing.T) {
	trace := &notificationConnectTrace{evaluated: make(chan struct{}), release: make(chan struct{})}
	writer, queue := notificationConnectFixture(t, trace)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	release := sync.OnceFunc(func() { close(trace.release) })
	defer release()
	conn, err := queue.db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	early := queueNotificationConnect(ctx, queue, "t1", "u1")
	waitNotificationConnectQueue(t, queue, 1)
	early2 := queueNotificationConnect(ctx, queue, "t2", "u2")
	waitNotificationConnectQueue(t, queue, 2)
	conn.Close()
	select {
	case <-trace.evaluated:
	case <-ctx.Done():
		t.Fatal("batch did not finish its SELECT")
	}
	// The first batch has read the old period. A subscriber arriving after this
	// commit must skip the new, already-visible period on its SSE stream.
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	writer.MustExec("UPDATE posting_windows SET starts_at=$1 WHERE id='open'", want)
	late := queueNotificationConnect(ctx, queue, "t1", "u1")
	waitNotificationConnectQueue(t, queue, 1)
	release()
	for i, reply := range []<-chan notificationConnectReply{early, early2, late} {
		r := <-reply
		expected := want.Add(-24 * time.Hour)
		if i == 2 {
			expected = want
		}
		if r.err != nil || r.state.ActiveWindowStartsAt == nil || !r.state.ActiveWindowStartsAt.Equal(expected) {
			t.Fatalf("subscription %d: startsAt=%v err=%v, want %s", i, r.state.ActiveWindowStartsAt, r.err, expected)
		}
	}
}

func TestNotificationConnectAllCanceledOnPostgres(t *testing.T) {
	trace := &notificationConnectTrace{}
	_, queue := notificationConnectFixture(t, trace)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := queue.db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waiting, stop := context.WithCancel(ctx)
	defer stop()
	first := queueNotificationConnect(waiting, queue, "t1", "u1")
	second := queueNotificationConnect(waiting, queue, "t2", "u2")
	waitNotificationConnectQueue(t, queue, 2)
	queue.mu.Lock()
	batch := queue.pending
	queue.mu.Unlock()

	stop()
	for _, reply := range []<-chan notificationConnectReply{first, second} {
		if r := <-reply; !errors.Is(r.err, context.Canceled) {
			t.Fatalf("disconnected client: %v", r.err)
		}
	}
	// Keep the sole DB connection occupied. Completion here proves the worker
	// left the pool wait instead of acquiring an unused connection later.
	select {
	case <-batch.done:
	case <-ctx.Done():
		t.Fatal("worker still waiting on the DB after all clients canceled")
	}
	if trace.singles.Load() != 0 || trace.batches.Load() != 0 {
		t.Fatal("disconnected clients executed SQL")
	}
}
