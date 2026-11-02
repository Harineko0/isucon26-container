package isureal

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
)

type journalProbe struct {
	bytes.Buffer
	hold  chan struct{}
	calls int
	max   int
	err   error
}

func (j *journalProbe) Write(p []byte) (int, error) {
	j.calls++
	if j.calls == 1 {
		<-j.hold
		return j.Buffer.Write(p)
	}
	if j.max > 0 && len(p) > j.max {
		n, _ := j.Buffer.Write(p[:j.max])
		return n, j.err
	}
	return j.Buffer.Write(p)
}

func TestActionLogWriteCombinesOnlyPendingLines(t *testing.T) {
	failure := errors.New("journal disconnected")
	for _, tc := range []struct {
		name string
		max  int
		err  error
		want string
	}{
		{"complete", 0, nil, "0\n1\n2\n3\n"},
		{"partial", 3, failure, "0\n1\n2"},
		{"short", 3, nil, "0\n1\n2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dst := &journalProbe{hold: make(chan struct{}), max: tc.max, err: tc.err}
				w := &actionLogWriter{output: dst}
				var wg sync.WaitGroup
				var written [4]int
				var errs [4]error
				for i := range written {
					wg.Go(func() { written[i], errs[i] = w.Write([]byte{byte('0' + i), '\n'}) })
					synctest.Wait()
				}
				if dst.calls != 1 {
					t.Fatalf("writes while blocked: %d", dst.calls)
				}
				close(dst.hold)
				wg.Wait()
				if dst.calls != 2 || dst.String() != tc.want {
					t.Fatalf("writes=%d, output=%q", dst.calls, dst.String())
				}
				for i := range written {
					wantN := min(2, max(0, len(tc.want)-2*i))
					var wantErr error
					if wantN < 2 {
						wantErr = tc.err
						if wantErr == nil {
							wantErr = io.ErrShortWrite
						}
					}
					if written[i] != wantN || !errors.Is(errs[i], wantErr) {
						t.Errorf("line %d: (%d,%v), want (%d,%v)", i, written[i], errs[i], wantN, wantErr)
					}
				}
			})
		})
	}
}
