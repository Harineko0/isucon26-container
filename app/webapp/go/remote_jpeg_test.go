package isureal

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/isureal-company/isureal/internal/jpegworker"
)

func TestOverflowJPEGValidation(t *testing.T) {
	credential := jpegworker.Credential("test secret")
	worker := httptest.NewServer(jpegworker.Handler(credential))
	t.Cleanup(worker.Close)
	oldURL, oldCredential := jpegWorkerURL, jpegWorkerCredential
	jpegWorkerURL, jpegWorkerCredential = worker.URL, credential
	t.Cleanup(func() { jpegWorkerURL, jpegWorkerCredential = oldURL, oldCredential })
	// Occupy the local decoder so the request exercises the HTTP path.
	installTestAdmission(t, 1, func([]byte) (int, int, error) {
		t.Fatal("local decoder used with available worker")
		return 0, 0, nil
	})
	if err := jpegValidation.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer jpegValidation.release()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 80, 45)), nil); err != nil {
		t.Fatal(err)
	}
	buf.Write(make([]byte, 64<<10))
	width, height, err := validateJPEGBytes(t.Context(), buf.Bytes())
	if err != nil || width != 80 || height != 45 {
		t.Fatalf("worker dimensions %dx%d, err=%v", width, height, err)
	}
	if _, _, err := validateJPEGBytes(t.Context(), bytes.Repeat([]byte{0}, 64<<10)); !errors.Is(err, errWorkerInvalidJPEG) {
		t.Fatalf("corrupt JPEG: %v", err)
	}
	jpegWorkerCredential = "wrong"
	if _, _, _, ok := validateJPEGOnWorker(t.Context(), buf.Bytes()); ok {
		t.Fatal("worker accepted invalid credential")
	}

}

func TestOverflowJPEGUnavailable(t *testing.T) {
	oldURL, oldCredential := jpegWorkerURL, jpegWorkerCredential
	defer func() { jpegWorkerURL, jpegWorkerCredential = oldURL, oldCredential }()
	called := make(chan struct{})
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		close(called)
	}))
	t.Cleanup(worker.Close)
	jpegWorkerURL, jpegWorkerCredential = worker.URL, "local-test"
	installTestAdmission(t, 1, func([]byte) (int, int, error) { return 23, 42, nil })
	if err := jpegValidation.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	go func() { <-called; jpegValidation.release() }()
	width, height, err := validateJPEGBytes(t.Context(), make([]byte, 64<<10))
	if err != nil || width != 23 || height != 42 {
		t.Fatalf("local fallback dimensions %dx%d, err=%v", width, height, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := validateJPEGBytes(ctx, make([]byte, 64<<10)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled overflow check: %v", err)
	}
}
