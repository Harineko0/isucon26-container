package isureal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func TestCreatePostGateLimitsConcurrencyAndReleases(t *testing.T) {
	g := make(createPostGate, 2)
	ctx := context.Background()
	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	// 3 本目は枠が空くまで待ち、取り消されたら DB に行かずに戻る。
	timeout, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := g.acquire(timeout); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded while the gate is full, got %v", err)
	}
	g.release()
	done := make(chan error, 1)
	go func() { done <- g.acquire(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("acquire must succeed after a release")
	}
	g.release()
	g.release()
	if len(g) != 0 {
		t.Fatalf("gate must be empty after releases, got %d", len(g))
	}
}

func TestNewCreatePostGateSizes(t *testing.T) {
	for region, want := range map[string]int{"asia": 24, "europe": 12, "america": 12, "other": 6} {
		if got := cap(newCreatePostGate(region)); got != want {
			t.Fatalf("%s: want %d, got %d", region, want, got)
		}
	}
}

func TestFileBackedPostsRetainAdmissionWhenSharedPoolIsBusy(t *testing.T) {
	request, primary := new(sqlx.DB), new(sqlx.DB)
	pools := newPostWritePools("asia", request, primary)
	ctx := t.Context()

	db, slot, err := pools.acquire(ctx, true)
	if err != nil || db != primary || slot != pools.primarySlots {
		t.Fatalf("did not use available primary slot: db=%p err=%v", db, err)
	}
	slot.release()

	// A DB fallback may carry a large bytea. Fill every shared slot with
	// these and check that only file-backed inserts use the two free slots.
	for range 22 {
		db, _, err := pools.acquire(ctx, false)
		if err != nil || db != request {
			t.Fatalf("fallback admission: db=%p err=%v", db, err)
		}
	}
	for range 2 {
		db, _, err := pools.acquire(ctx, true)
		if err != nil || db != primary {
			t.Fatalf("file-backed admission: db=%p err=%v", db, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := pools.acquire(cancelled, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("admitted a 25th post: %v", err)
	}
	pools.primarySlots.release()
	if _, _, err := pools.acquire(cancelled, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("bytea fallback admitted to the primary: %v", err)
	}
	pools.requestSlots.release()
	db, slot, err = pools.acquire(ctx, false)
	if db != request || slot != pools.requestSlots || err != nil {
		t.Fatalf("unexpected fallback slot with shared capacity: db=%p err=%v", db, err)
	}
	for _, region := range []string{"asia", "europe", "america"} {
		for _, direct := range []*sqlx.DB{request, primary} {
			if region == "asia" && direct != request {
				continue
			}
			p := newPostWritePools(region, request, direct)
			if p.primarySlots != nil || cap(p.requestSlots) != createPostGateSize[region] {
				t.Fatalf("unexpected split for %s (separate pool: %t)", region, direct != request)
			}
		}
	}
}
