package isureal

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// chunkReader hands out at most chunk bytes per Read, like a socket, and hides
// Len() so neither path can cheat by inspecting the source.
type chunkReader struct {
	r     io.Reader
	chunk int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.chunk {
		p = p[:c.chunk]
	}
	return c.r.Read(p)
}

func randomBody(n int) []byte {
	rng := rand.New(rand.NewPCG(7, 11))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.IntN(256))
	}
	return b
}

func TestReadUpload(t *testing.T) {
	body := randomBody(360 << 10)
	tests := []struct {
		name    string
		body    []byte
		size    int64
		wantErr error
		wantLen int
	}{
		{name: "exact size", body: body, size: int64(len(body)), wantLen: len(body)},
		{name: "unknown size", body: body, size: -1, wantLen: len(body)},
		{name: "zero size with empty body", body: nil, size: 0, wantLen: 0},
		{name: "size declared larger than body", body: body, size: int64(len(body)) + 1, wantErr: io.ErrUnexpectedEOF},
		{name: "size declared smaller than body", body: body, size: int64(len(body)) - 1000, wantLen: len(body)},
		{name: "size declared above cap falls back and reads", body: body, size: maxUploadBytes + 1, wantLen: len(body)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readUpload(&chunkReader{r: bytes.NewReader(tt.body), chunk: 16 << 10}, tt.size)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tt.body) {
				t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(tt.body))
			}
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tt.wantLen)
			}
			if tt.size > 0 && tt.size == int64(len(tt.body)) && cap(got) != len(got) {
				t.Errorf("cap = %d, want exactly len %d when size is known", cap(got), len(got))
			}
		})
	}
}

func TestReadUpload_TooLarge(t *testing.T) {
	// Only reachable if nginx's client_max_body_size were raised: bounded growth, 413.
	huge := &chunkReader{r: io.LimitReader(zeroReader{}, maxUploadBytes+5), chunk: 1 << 20}
	_, err := readUpload(huge, -1)
	if !errors.Is(err, errUploadTooLarge) {
		t.Fatalf("unknown size over cap: err = %v, want errUploadTooLarge", err)
	}
	huge = &chunkReader{r: io.LimitReader(zeroReader{}, maxUploadBytes+5), chunk: 1 << 20}
	_, err = readUpload(huge, maxUploadBytes-1)
	if !errors.Is(err, errUploadTooLarge) {
		t.Fatalf("lying size over cap: err = %v, want errUploadTooLarge", err)
	}
}

func TestLargeProfileUpload(t *testing.T) {
	const size = 3 << 20
	payload := bytes.Repeat([]byte{0x01}, size)
	body := bytes.NewReader(payload)
	got, err := readProfileUpload(body, size)
	if err != nil || len(got) != 0 || body.Len() != 0 {
		t.Fatalf("invalid JPEG: kept %d bytes, unread %d, error %v", len(got), body.Len(), err)
	}
	// A possible JPEG must reach the full validator, including all trailing bytes.
	payload[0], payload[1] = 0xff, 0xd8
	got, err = readProfileUpload(&chunkReader{r: bytes.NewReader(payload), chunk: 1}, size)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("JPEG input changed: got %d bytes, error %v", len(got), err)
	}
	for _, n := range []int64{0, 1, size - 1, maxUploadBytes + 1} {
		wantErr := io.ErrUnexpectedEOF
		if n > maxUploadBytes {
			wantErr = errUploadTooLarge
		}
		if _, err := readProfileUpload(io.LimitReader(zeroReader{}, n), size); !errors.Is(err, wantErr) {
			t.Errorf("body length %d: error %v, want %v", n, err, wantErr)
		}
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

func benchmarkRead(b *testing.B, n int, read func(r io.Reader, size int64) ([]byte, error)) {
	body := randomBody(n)
	b.SetBytes(int64(n))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := read(&chunkReader{r: bytes.NewReader(body), chunk: 32 << 10}, int64(n))
		if err != nil || len(got) != n {
			b.Fatal(err, len(got))
		}
	}
}

func readAllIgnoringSize(r io.Reader, _ int64) ([]byte, error) { return io.ReadAll(r) }

func BenchmarkReadAll_360KB(b *testing.B)    { benchmarkRead(b, 360<<10, readAllIgnoringSize) }
func BenchmarkReadUpload_360KB(b *testing.B) { benchmarkRead(b, 360<<10, readUpload) }
func BenchmarkReadAll_900KB(b *testing.B)    { benchmarkRead(b, 900<<10, readAllIgnoringSize) }
func BenchmarkReadUpload_900KB(b *testing.B) { benchmarkRead(b, 900<<10, readUpload) }
func BenchmarkReadAll_8MB(b *testing.B)      { benchmarkRead(b, 8<<20, readAllIgnoringSize) }
func BenchmarkReadUpload_8MB(b *testing.B)   { benchmarkRead(b, 8<<20, readUpload) }
