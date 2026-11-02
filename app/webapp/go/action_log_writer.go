package isureal

import (
	"io"
	"sync"
)

// Write complete JSON lines to the same journal stream in arrival order.
// A caller joins only a write that has not started. Its Write returns after
// the underlying writer has accepted its bytes, including on partial failure.
type actionLogWriter struct {
	output  io.Writer
	mu      sync.Mutex
	busy    bool
	pending *actionLogBatch
}

type actionLogBatch struct {
	data        []byte
	ready, done chan struct{}
	n           int
	err         error
}

func (w *actionLogWriter) write(data []byte) (int, error) {
	n, err := w.output.Write(data)
	if n != len(data) && err == nil {
		err = io.ErrShortWrite
	}
	w.mu.Lock()
	if next := w.pending; next != nil {
		w.pending = nil
		close(next.ready)
	} else {
		w.busy = false
	}
	w.mu.Unlock()
	return n, err
}

func (w *actionLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if !w.busy {
		w.busy = true
		w.mu.Unlock()
		return w.write(p)
	}
	b := w.pending
	start := 0
	if b == nil {
		// Reuse the first caller's bytes without appending into its spare capacity.
		b = &actionLogBatch{data: p[:len(p):len(p)], ready: make(chan struct{}), done: make(chan struct{})}
		w.pending = b
		w.mu.Unlock()
		<-b.ready
		b.n, b.err = w.write(b.data)
		close(b.done)
	} else {
		start = len(b.data)
		b.data = append(b.data, p...)
		w.mu.Unlock()
		<-b.done
	}

	n := min(len(p), max(0, b.n-start))
	if n < len(p) || start+len(p) == len(b.data) {
		return n, b.err
	}
	return n, nil
}
