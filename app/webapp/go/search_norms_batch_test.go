package isureal

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

// このモジュールは testify を使っていない (依存を増やさない) ので、標準の比較で確かめる。

func TestCollectSearchNormsBatch(t *testing.T) {
	tests := map[string]struct {
		in    []User
		close bool
		max   int
		want  []User
	}{
		"dedupes by id, keeps the last display name, keeps first-seen order": {
			in:   []User{{ID: "a", DisplayName: "A1"}, {ID: "b", DisplayName: "B"}, {ID: "a", DisplayName: "A2"}},
			max:  10,
			want: []User{{ID: "a", DisplayName: "A2"}, {ID: "b", DisplayName: "B"}},
		},
		"stops at max distinct users": {
			in:   []User{{ID: "a"}, {ID: "b"}, {ID: "c"}},
			max:  2,
			want: []User{{ID: "a"}, {ID: "b"}},
		},
		"duplicates do not count toward max": {
			in:   []User{{ID: "a"}, {ID: "a"}, {ID: "b"}},
			max:  2,
			want: []User{{ID: "a"}, {ID: "b"}},
		},
		"closed channel returns what was queued": {
			in:    []User{{ID: "a"}},
			close: true,
			max:   10,
			want:  []User{{ID: "a"}},
		},
		"closed and empty returns nil": {
			close: true,
			max:   10,
			want:  nil,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ch := make(chan User, len(tt.in))
			for _, u := range tt.in {
				ch <- u
			}
			if tt.close {
				close(ch)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			got := collectSearchNormsBatch(ctx, ch, tt.max, 10*time.Millisecond)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// 最初の 1 件のあと wait が過ぎたら、max に届いていなくてもその時点の分を返す。
// wait より後に来た分は次のまとまりになる。
func TestCollectSearchNormsBatch_FlushesAfterWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan User, 10)
		ch <- User{ID: "a"}
		go func() {
			time.Sleep(50 * time.Millisecond)
			ch <- User{ID: "b"}
			time.Sleep(100 * time.Millisecond) // wait (100ms) を過ぎてから
			ch <- User{ID: "c"}
		}()

		first := collectSearchNormsBatch(t.Context(), ch, 10, 100*time.Millisecond)
		second := collectSearchNormsBatch(t.Context(), ch, 10, 100*time.Millisecond)

		if want := []User{{ID: "a"}, {ID: "b"}}; !reflect.DeepEqual(first, want) {
			t.Errorf("first batch = %+v, want %+v", first, want)
		}
		if want := []User{{ID: "c"}}; !reflect.DeepEqual(second, want) {
			t.Errorf("second batch = %+v, want %+v", second, want)
		}
	})
}

// ctx が終わったら、空のまま待っていても nil を返して止まる (run のゴルーチンが残らない)。
func TestCollectSearchNormsBatch_StopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		ch := make(chan User)
		done := make(chan []User)
		go func() { done <- collectSearchNormsBatch(ctx, ch, 10, time.Second) }()

		synctest.Wait()
		cancel()

		if got := <-done; got != nil {
			t.Errorf("got %+v after cancel, want nil", got)
		}
	})
}

func TestSearchNormsSaver_Enqueue_FullQueueDoesNotBlock(t *testing.T) {
	s := &searchNormsSaver{queue: make(chan User, 1)}

	s.enqueue(User{ID: "a"})
	s.enqueue(User{ID: "b"}) // 満杯でも呼び出し側 (応答) を待たせない

	if got := len(s.queue); got != 1 {
		t.Errorf("queue length = %d, want 1", got)
	}
}
