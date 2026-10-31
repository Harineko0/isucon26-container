package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func TestHeavyReadsQueueBeforeDBCheckout(t *testing.T) {
	started := make(chan struct{}, 4)
	unblock := make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	connector := &recordingConnector{respond: func(query string) ([]string, [][]driver.Value) {
		if query == "slow" {
			started <- struct{}{}
			<-unblock
		}
		return []string{"n"}, [][]driver.Value{{int64(1)}}
	}}
	db := sqlx.NewDb(sql.OpenDB(connector), "pgx")
	defer db.Close()
	defer release()
	h := &HandlerContext{DB: db, heavyReads: newHeavyReadSlots("asia")}
	n := cap(h.heavyReads)
	db.SetMaxOpenConns(n + 1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	done := make(chan error, n)
	for range n {
		go func() { done <- h.selectHeavy(ctx, new([]int), "slow") }()
	}
	for range n {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("slow reads did not fill the gate")
		}
	}
	wait, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if err := h.selectHeavy(wait, new([]int), "queued"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting read returned %v", err)
	}
	if err := db.GetContext(ctx, new(int), "short"); err != nil {
		t.Fatal(err)
	}
	if db.Stats().WaitCount != 0 || len(connector.recorded()) != n+1 {
		t.Fatalf("queued read reached the pool: %+v, queries %v", db.Stats(), connector.recorded())
	}
	release()
	for range n {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// A result decoding failure must return the slot too.
	if err := h.selectHeavy(ctx, new([]time.Time), "bad result type"); err == nil {
		t.Fatal("expected scan error")
	}
	if len(h.heavyReads) != 0 {
		t.Fatal("slot leaked after read")
	}
}
