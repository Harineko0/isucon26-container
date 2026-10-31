package isureal

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v5"
)

// A second write to the pipe proves the file writer consumed the first chunk.
// A concurrent reader must still see no published replica until EOF and fsync.
func TestReplicaPUTPublishesOnlyACompleteImage(t *testing.T) {
	setupShardedStore(t)
	oldSecret := internalSecret.Load()
	t.Cleanup(func() { internalSecret.Store(oldSecret) })
	setInternalSecret(&Config{DBPassword: "test-secret"})
	e := echo.New()
	e.Pre(InternalMiddleware(&HandlerContext{}))

	for _, tc := range []struct {
		name    string
		readErr error
		status  int
	}{
		{"complete", nil, http.StatusNoContent},
		{"truncated", io.ErrUnexpectedEOF, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := newImageID("america")
			path, _ := imageStorePath(id)
			pr, pw := io.Pipe()
			defer pr.Close()
			defer pw.Close()
			req := httptest.NewRequest(http.MethodPut, "/api/internal/images/"+id, pr)
			req.Header.Set(internalSecretHeader, *internalSecret.Load())
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer pr.Close()
				e.ServeHTTP(rec, req)
			}()
			chunk := bytes.Repeat([]byte{0xbd}, 32<<10)
			for range 2 {
				if _, err := pw.Write(chunk); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unfinished image visible: %v", err)
			}
			temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".tmp-*"))
			if err != nil || len(temps) != 1 {
				t.Fatalf("stream did not open exactly one temporary file: %v %v", temps, err)
			}
			if fi, err := os.Stat(temps[0]); err != nil || fi.Size() < int64(len(chunk)) {
				t.Fatalf("first chunk not written before EOF: %v %v", fi, err)
			}
			pw.CloseWithError(tc.readErr)
			<-done
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d", rec.Code, tc.status)
			}
			if _, err := os.Stat(temps[0]); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary file survived: %v", err)
			}
			if tc.readErr != nil {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("partial replica published: %v", err)
				}
				return
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, bytes.Repeat(chunk, 2)) {
				t.Fatalf("replica contents differ: %v", err)
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
				t.Fatalf("replica permissions: %v %v", fi, err)
			}
		})
	}
	for _, tc := range []struct {
		length int64
		body   string
	}{{0, ""}, {maxPeerImageBytes + 1, "data"}} {
		id := newImageID("europe")
		path, _ := imageStorePath(id)
		req := httptest.NewRequest(http.MethodPut, "/api/internal/images/"+id, bytes.NewBufferString(tc.body))
		req.ContentLength = tc.length
		req.Header.Set(internalSecretHeader, *internalSecret.Load())
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("size %d: status %d", tc.length, rec.Code)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("size %d published a file: %v", tc.length, err)
		}
	}
}
