package isureal

import (
	"context"
	"errors"
	"testing"
	"time"
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
	for region, want := range map[string]int{"asia": 8, "europe": 4, "america": 4, "other": 6} {
		if got := cap(newCreatePostGate(region)); got != want {
			t.Fatalf("%s: want %d, got %d", region, want, got)
		}
	}
}
