package isureal

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func installTestAdmission(t *testing.T, n int, validate func([]byte) (int, int, error)) {
	t.Helper()
	oldAdmission, oldValidate := jpegValidation, validateJPEGFunc
	jpegValidation = newCPUAdmission(n)
	if validate != nil {
		validateJPEGFunc = validate
	}
	t.Cleanup(func() { jpegValidation, validateJPEGFunc = oldAdmission, oldValidate })
}

func TestValidateJPEGBytesBoundsConcurrentValidations(t *testing.T) {
	var running, peak, calls atomic.Int32
	installTestAdmission(t, 2, func(data []byte) (int, int, error) {
		calls.Add(1)
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		running.Add(-1)
		return len(data), 1, nil
	})
	var wg sync.WaitGroup
	results := make([]int, 40)
	for i := range results {
		wg.Go(func() {
			w, h, err := validateJPEGBytes(context.Background(), make([]byte, i+1))
			if err != nil || h != 1 {
				t.Errorf("validation %d: %d %d %v", i, w, h, err)
			}
			results[i] = w
		})
	}
	wg.Wait()
	for i, w := range results {
		if w != i+1 {
			t.Fatalf("result %d = %d", i, w)
		}
	}
	if calls.Load() != 40 {
		t.Fatalf("validations = %d, want 40", calls.Load())
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency = %d, want exactly the admission size 2", peak.Load())
	}
}

func TestValidateJPEGBytesCanceledWhileWaitingDoesNotValidate(t *testing.T) {
	var calls atomic.Int32
	installTestAdmission(t, 1, func([]byte) (int, int, error) { calls.Add(1); return 1, 1, nil })
	if err := jpegValidation.acquire(context.Background()); err != nil { // 枠を埋める
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := validateJPEGBytes(canceled, []byte("jpeg")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	timeout, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	start := time.Now()
	if _, _, err := validateJPEGBytes(timeout, []byte("jpeg")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("waited past the deadline")
	}
	if calls.Load() != 0 {
		t.Fatalf("validated %d images while the slot was taken and the request was gone", calls.Load())
	}
	jpegValidation.release()
	if _, _, err := validateJPEGBytes(context.Background(), []byte("jpeg")); err != nil || calls.Load() != 1 {
		t.Fatalf("validation after release: err=%v calls=%d", err, calls.Load())
	}
	// 取り消し済みの要求は、枠が空いていても検証しない。
	if _, _, err := validateJPEGBytes(canceled, []byte("jpeg")); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("canceled request was validated: err=%v calls=%d", err, calls.Load())
	}
}

// 実際の検証器: 不正な JPEG は jpegcheck の誤り (ctx は生きている)、正しい JPEG は寸法を返す。
func TestValidateJPEGBytesUsesJPEGCheck(t *testing.T) {
	ctx := context.Background()
	if _, _, err := validateJPEGBytes(ctx, []byte("not a jpeg")); err == nil || ctx.Err() != nil {
		t.Fatalf("invalid JPEG accepted: %v", err)
	}
	var img bytes.Buffer
	if err := jpeg.Encode(&img, image.NewGray(image.Rect(0, 0, 24, 16)), nil); err != nil {
		t.Fatal(err)
	}
	w, h, err := validateJPEGBytes(ctx, img.Bytes())
	if err != nil || w != 24 || h != 16 {
		t.Fatalf("valid JPEG: %d×%d %v", w, h, err)
	}
	if cap(jpegValidation) < 1 {
		t.Fatalf("admission size %d", cap(jpegValidation))
	}
}
