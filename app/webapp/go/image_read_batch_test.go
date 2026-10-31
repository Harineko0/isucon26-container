package isureal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestImageReadsSeparateSnapshotsAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type query struct {
			ctx   context.Context
			count int
			reply chan bool
		}
		queries := make(chan query, 3)
		r := &imageReads{load: func(ctx context.Context, _ string, takeRequests func() []imageReadRequest) []imageReadResult {
			requests := takeRequests()
			q := query{ctx: ctx, count: len(requests), reply: make(chan bool)}
			queries <- q
			results := make([]imageReadResult, len(requests))
			select {
			case allowed := <-q.reply:
				for i := range results {
					results[i].object.Allowed = allowed
				}
			case <-ctx.Done():
				for i := range results {
					results[i].err = ctx.Err()
				}
			}
			return results
		}}
		get := func(ctx context.Context) <-chan imageReadResult {
			out := make(chan imageReadResult, 1)
			go func() {
				object, err := r.get(ctx, "leaf", imageReadRequest{id: "same-image"})
				out <- imageReadResult{object, err}
			}()
			return out
		}
		ctx, cancel := context.WithCancel(t.Context())
		lost, first := get(ctx), get(t.Context())
		synctest.Wait()
		time.Sleep(2 * time.Millisecond)
		q1 := <-queries
		if q1.count != 2 {
			t.Fatalf("same-image requests were not batched: %d", q1.count)
		}
		cancel()
		if got := <-lost; !errors.Is(got.err, context.Canceled) {
			t.Fatal("cancelled request did not leave")
		}
		if q1.ctx.Err() != nil {
			t.Fatal("one caller cancelled a peer's query")
		}
		// A deletion commits here. A later request for the same ID must use
		// the new snapshot even if the first SELECT is still in flight.
		later := get(t.Context())
		synctest.Wait()
		time.Sleep(2 * time.Millisecond)
		q2 := <-queries
		q2.reply <- false
		q1.reply <- true
		if a, b := <-first, <-later; a.err != nil || b.err != nil || !a.object.Allowed || b.object.Allowed {
			t.Fatalf("old snapshot reused: before=%+v after=%+v", a, b)
		}
		ctx2, cancel2 := context.WithCancel(t.Context())
		last := get(ctx2)
		synctest.Wait()
		time.Sleep(2 * time.Millisecond)
		q3 := <-queries
		cancel2()
		<-last
		synctest.Wait()
		if q3.ctx.Err() == nil {
			t.Fatal("all waiters cancelled but the query survived")
		}
	})
}

func TestImageReadsAdmitDuringPoolWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type checkout struct {
			rel     string
			release chan struct{}
		}
		checkouts := make(chan checkout, 3)
		counts := make(chan int, 3)
		r := &imageReads{load: func(ctx context.Context, rel string, takeRequests func() []imageReadRequest) []imageReadResult {
			c := checkout{rel, make(chan struct{})}
			checkouts <- c
			select {
			case <-ctx.Done():
			case <-c.release:
			}
			requests := takeRequests()
			counts <- len(requests)
			results := make([]imageReadResult, len(requests))
			for i, req := range requests {
				results[i].object.MediaType = req.id
				results[i].err = ctx.Err()
			}
			return results
		}}
		get := func(ctx context.Context, rel, id string) <-chan imageReadResult {
			out := make(chan imageReadResult, 1)
			go func() {
				obj, err := r.get(ctx, rel, imageReadRequest{id: id})
				out <- imageReadResult{obj, err}
			}()
			return out
		}
		first := get(t.Context(), "leaf-a", "A")
		held := <-checkouts
		if held.rel != "leaf-a" {
			t.Fatal(held.rel)
		}
		// The initial 1 ms window is long over but no SELECT has started.
		time.Sleep(5 * time.Millisecond)
		late := get(t.Context(), "leaf-a", "B")
		ctx, cancel := context.WithCancel(t.Context())
		other := get(ctx, "leaf-b", "C")
		second := <-checkouts
		if second.rel != "leaf-b" {
			t.Fatalf("queued requests for leaf-a started another checkout: %s", second.rel)
		}
		cancel()
		if got := <-other; !errors.Is(got.err, context.Canceled) {
			t.Fatalf("last waiter cancellation during checkout: %+v", got)
		}
		if n := <-counts; n != 1 {
			t.Fatalf("canceled partition took %d requests", n)
		}
		close(held.release)
		if n := <-counts; n != 2 {
			t.Fatalf("expected 2 requests on the held connection, got %d", n)
		}
		for id, out := range map[string]<-chan imageReadResult{"A": first, "B": late} {
			if got := <-out; got.err != nil || got.object.MediaType != id {
				t.Fatalf("wrong result for %s: %+v", id, got)
			}
		}
	})
}

func TestImageReadsBoundBatchSizeAndSeparateRelations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		counts := map[string][]int{}
		r := &imageReads{load: func(_ context.Context, rel string, takeRequests func() []imageReadRequest) []imageReadResult {
			requests := takeRequests()
			mu.Lock()
			counts[rel] = append(counts[rel], len(requests))
			mu.Unlock()
			results := make([]imageReadResult, len(requests))
			for i, req := range requests {
				results[i].object.MediaType = req.id
			}
			return results
		}}
		var wg sync.WaitGroup
		for i := range 150 {
			wg.Go(func() {
				id := fmt.Sprint(i)
				rel := "leaf-a"
				if i < 10 {
					rel = "leaf-b"
				}
				object, err := r.get(t.Context(), rel, imageReadRequest{id: id})
				if err != nil || object.MediaType != id {
					t.Errorf("request received another request's result: %s %+v %v", id, object, err)
				}
			})
		}
		wg.Wait()
		if len(counts["leaf-a"]) != 3 || len(counts["leaf-b"]) != 1 {
			t.Fatalf("unexpected grouping: %v", counts)
		}
		for _, batches := range counts {
			for _, count := range batches {
				if count > maxImageReadBatch {
					t.Fatal("batch exceeded its bound")
				}
			}
		}
	})
}

func TestImageBatchQueryUsesOnePairedInputPerRequest(t *testing.T) {
	for _, rel := range []string{"image_objects", "image_objects_202601", "image_objects_20261028_p3"} {
		q := imageBatchQueryFor(rel)
		if strings.Count(q, "$1") != 1 || strings.Count(q, "$2") != 1 || !strings.Contains(q, "JOIN "+rel+" i ON i.id = request.id") || !strings.Contains(q, "CASE WHEN request.has_data THEN NULL ELSE i.data END") || !strings.Contains(q, "request.ordinality AS request_index") {
			t.Fatalf("batch input transformation failed for %s: %s", rel, q)
		}
	}
}
