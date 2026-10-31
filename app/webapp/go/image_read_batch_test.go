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
		r := &imageReads{load: func(ctx context.Context, _ string, requests []imageReadRequest) []imageReadResult {
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

func TestImageReadsBoundBatchSizeAndSeparateRelations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		counts := map[string][]int{}
		r := &imageReads{load: func(_ context.Context, rel string, requests []imageReadRequest) []imageReadResult {
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
