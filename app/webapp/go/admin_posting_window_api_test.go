package isureal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 期間のコミット後の通知 INSERT (#28): 一時的な失敗はやり直し、成功したら止まる。
func TestRetryDetachedRetriesUntilSuccess(t *testing.T) {
	var calls []int
	var lasts []error
	boom := errors.New("boom")
	err := retryDetached(context.Background(), time.Second, 3, func(ctx context.Context, attempt int, last error) error {
		calls = append(calls, attempt)
		lasts = append(lasts, last)
		if attempt < 2 {
			return boom
		}
		return nil
	})
	if err != nil {
		t.Fatalf("want success, got %v", err)
	}
	if len(calls) != 2 || calls[0] != 1 || calls[1] != 2 {
		t.Fatalf("want attempts [1 2], got %v", calls)
	}
	if lasts[0] != nil || !errors.Is(lasts[1], boom) {
		t.Fatalf("want last errors [nil boom], got %v", lasts)
	}
}

func TestRetryDetachedGivesUpAfterAttempts(t *testing.T) {
	boom := errors.New("boom")
	n := 0
	err := retryDetached(context.Background(), time.Second, 3, func(ctx context.Context, attempt int, last error) error {
		n++
		return boom
	})
	if !errors.Is(err, boom) || n != 3 {
		t.Fatalf("want boom after 3 attempts, got %v after %d", err, n)
	}
}

// 管理クライアントが切断してリクエストの context が取り消されても、期間はもう開いているので
// 通知の INSERT は続ける。
func TestRetryDetachedIgnoresParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	err := retryDetached(parent, time.Second, 3, func(ctx context.Context, attempt int, last error) error {
		n++
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("want one successful attempt despite cancelled parent, got err=%v n=%d", err, n)
	}
}

func TestRetryDetachedStopsAtTimeout(t *testing.T) {
	n := 0
	err := retryDetached(context.Background(), 20*time.Millisecond, 100, func(ctx context.Context, attempt int, last error) error {
		n++
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || n != 1 {
		t.Fatalf("want DeadlineExceeded after 1 attempt, got err=%v n=%d", err, n)
	}
}
