package isureal

import (
	"context"
	"sync"
	"time"
)

// A batch accepts requests only before its SELECT starts. In particular, a
// request following a committed write can never join an older in-flight read.
type windowReads struct {
	mu      sync.Mutex
	pending *windowReadBatch
	load    func(context.Context, []string) (map[string]currentPostingWindowState, error)
}

type windowReadBatch struct {
	tokens  []string
	waiters int
	cancel  context.CancelFunc
	full    chan struct{}
	done    chan struct{}
	rows    map[string]currentPostingWindowState
	err     error
}

const maxWindowBatch = 128

func (r *windowReads) get(ctx context.Context, token string) (currentPostingWindowState, bool, error) {
	if err := ctx.Err(); err != nil {
		return currentPostingWindowState{}, false, err
	}
	r.mu.Lock()
	b := r.pending
	if b == nil {
		queryCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		b = &windowReadBatch{cancel: cancel, full: make(chan struct{}), done: make(chan struct{})}
		r.pending = b
		go r.read(queryCtx, b)
	}
	b.tokens = append(b.tokens, token)
	b.waiters++
	if len(b.tokens) == maxWindowBatch {
		r.pending = nil
		close(b.full)
	}
	r.mu.Unlock()
	select {
	case <-b.done:
		row, found := b.rows[token]
		return row, found, b.err
	case <-ctx.Done():
		r.mu.Lock()
		b.waiters--
		if b.waiters == 0 {
			if r.pending == b {
				r.pending = nil
			}
			b.cancel()
		}
		r.mu.Unlock()
		return currentPostingWindowState{}, false, ctx.Err()
	}
}

func (r *windowReads) read(ctx context.Context, b *windowReadBatch) {
	defer b.cancel()
	defer close(b.done)
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-b.full:
	case <-ctx.Done():
		return
	}
	r.mu.Lock()
	if r.pending == b {
		r.pending = nil
	}
	r.mu.Unlock()
	b.rows, b.err = r.load(ctx, b.tokens)
}
