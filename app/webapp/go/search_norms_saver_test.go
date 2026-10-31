package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// slowExecDriver は Exec に execDelay かかる DB の代わり。同時に実行中の Exec の数を数える。
type slowExecDriver struct {
	execDelay time.Duration
	active    atomic.Int64
	peak      atomic.Int64
	execs     atomic.Int64
}

func (d *slowExecDriver) Open(string) (driver.Conn, error) { return &slowExecConn{d: d}, nil }

type slowExecConn struct{ d *slowExecDriver }

func (*slowExecConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (*slowExecConn) Close() error                        { return nil }
func (*slowExecConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

// CheckNamedValue は saveSearchNorms の []string 引数をそのまま受ける。
func (*slowExecConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c *slowExecConn) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	n := c.d.active.Add(1)
	defer c.d.active.Add(-1)
	for {
		peak := c.d.peak.Load()
		if n <= peak || c.d.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	c.d.execs.Add(1)
	select {
	case <-time.After(c.d.execDelay):
		return driver.RowsAffected(1), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var slowExecDriverSeq atomic.Int64

func openSlowExecDB(t *testing.T, delay time.Duration, maxConns int) (*sqlx.DB, *slowExecDriver) {
	t.Helper()
	d := &slowExecDriver{execDelay: delay}
	name := fmt.Sprintf("isureal-slow-exec-%d", slowExecDriverSeq.Add(1))
	sql.Register(name, d)
	raw, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(maxConns)
	raw.SetMaxIdleConns(maxConns)
	db := sqlx.NewDb(raw, "pgx")
	t.Cleanup(func() { db.Close() })
	return db, d
}

// burstUsers は登録が同時に来たときのように、別々の利用者を 1 件ずつ返す。
func burstUsers(n int) [][]User {
	out := make([][]User, n)
	for i := range out {
		out[i] = []User{{ID: fmt.Sprintf("u%04d", i), Username: fmt.Sprintf("user%04d", i), DisplayName: fmt.Sprintf("User %d", i)}}
	}
	return out
}

// probeConnWait は保存の嵐の最中に来た軽い API (posting-window/current など) が、接続を得るまでに待つ時間を測る。
func probeConnWait(t *testing.T, db *sqlx.DB) time.Duration {
	t.Helper()
	start := time.Now()
	conn, err := db.Connx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wait := time.Since(start)
	conn.Close()
	return wait
}

// 以前の実装 (呼び出しごとに goroutine を立てて保存) と比べ、保存の嵐の間も保存が使う接続は 1 本だけで、
// 軽い API が接続を待たされないこと (#68)。
func TestSearchNormsSaverHoldsOneConnection(t *testing.T) {
	const (
		burst    = 64
		maxConns = 4
		delay    = 10 * time.Millisecond
	)

	t.Run("before: goroutine per call", func(t *testing.T) {
		db, d := openSlowExecDB(t, delay, maxConns)
		var wg sync.WaitGroup
		for _, users := range burstUsers(burst) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = saveSearchNorms(context.Background(), db, users)
			}()
		}
		for d.active.Load() < maxConns {
			time.Sleep(time.Millisecond)
		}
		wait := probeConnWait(t, db)
		wg.Wait()
		t.Logf("before: peak conns=%d statements=%d pool waits=%d probe wait=%v", d.peak.Load(), d.execs.Load(), db.Stats().WaitCount, wait)
		if d.peak.Load() != maxConns {
			t.Fatalf("baseline peak = %d, want the whole pool (%d)", d.peak.Load(), maxConns)
		}
	})

	t.Run("after: single saver", func(t *testing.T) {
		db, d := openSlowExecDB(t, delay, maxConns)
		saver := newSearchNormsSaver(func(ctx context.Context, users []User) error {
			return saveSearchNorms(ctx, db, users)
		})
		var wg sync.WaitGroup
		for _, users := range burstUsers(burst) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				saver.add(users)
			}()
		}
		wg.Wait()
		for d.active.Load() == 0 && d.execs.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		wait := probeConnWait(t, db)
		waitSaved(t, saver)
		t.Logf("after: peak conns=%d statements=%d pool waits=%d probe wait=%v", d.peak.Load(), d.execs.Load(), db.Stats().WaitCount, wait)
		if d.peak.Load() != 1 {
			t.Fatalf("peak connections used by saves = %d, want 1", d.peak.Load())
		}
		if got := d.execs.Load(); got > 3 {
			t.Fatalf("statements = %d for %d users, want them batched (<= 3)", got, burst)
		}
		if w := db.Stats().WaitCount; w != 0 {
			t.Fatalf("pool waits = %d, want 0", w)
		}
		if wait > delay {
			t.Fatalf("probe waited %v for a connection, want < %v", wait, delay)
		}
	})
}

// waitSaved は保存待ちも保存中も無くなるまで待つ。
func waitSaved(t *testing.T, s *searchNormsSaver) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		idle := len(s.pending) == 0 && len(s.inflight) == 0
		s.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("saver did not drain")
		}
		time.Sleep(time.Millisecond)
	}
}

// 保存の内容: 全員がちょうど 1 回保存され、保存待ちの間に名前が変われば新しい名前で保存され、
// 保存中に同じ名前で積まれた利用者は重ねて保存しない。保存中に名前が変われば保存し直す。
func TestSearchNormsSaverBatchesAndDedupes(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, 16)
	var mu sync.Mutex
	var batches [][]User
	saver := newSearchNormsSaver(func(_ context.Context, users []User) error {
		started <- struct{}{}
		<-release
		mu.Lock()
		batches = append(batches, append([]User(nil), users...))
		mu.Unlock()
		return nil
	})

	saver.add([]User{{ID: "a", DisplayName: "A1"}})
	<-started // "a" (A1) を保存中

	saver.add([]User{{ID: "a", DisplayName: "A1"}}) // 保存中と同じ → 積まない
	saver.add([]User{{ID: "c", DisplayName: "C1"}, {ID: "b", DisplayName: "B1"}})
	saver.add([]User{{ID: "b", DisplayName: "B2"}}) // 保存待ちの間に変わった → B2 で保存
	saver.add([]User{{ID: "a", DisplayName: "A2"}}) // 保存中に変わった → 保存し直す
	close(release)
	waitSaved(t, saver)

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 2 {
		t.Fatalf("batches = %v, want 2", batches)
	}
	want := [][]User{
		{{ID: "a", DisplayName: "A1"}},
		{{ID: "a", DisplayName: "A2"}, {ID: "b", DisplayName: "B2"}, {ID: "c", DisplayName: "C1"}},
	}
	for i := range want {
		if len(batches[i]) != len(want[i]) {
			t.Fatalf("batch %d = %v, want %v", i, batches[i], want[i])
		}
		for j := range want[i] {
			if batches[i][j].ID != want[i][j].ID || batches[i][j].DisplayName != want[i][j].DisplayName {
				t.Fatalf("batch %d = %v, want %v", i, batches[i], want[i])
			}
		}
	}
}

// 保存に失敗しても saver は止まらず、次に積まれた分を保存する。
func TestSearchNormsSaverContinuesAfterError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	saved := make(chan string, 4)
	saver := newSearchNormsSaver(func(_ context.Context, users []User) error {
		if calls.Add(1) == 1 {
			return errors.New("boom")
		}
		saved <- users[0].ID
		return nil
	})
	saver.add([]User{{ID: "x", DisplayName: "X"}})
	waitSaved(t, saver)
	saver.add([]User{{ID: "y", DisplayName: "Y"}})
	select {
	case id := <-saved:
		if id != "y" {
			t.Fatalf("saved %q, want y", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("saver stopped after an error")
	}
}
