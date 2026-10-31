package isureal

import (
	"context"
	"sync"

	"github.com/jmoiron/sqlx"
)

const maxFriendFeedBatch = 16

type friendFeedRequest struct {
	userID    string
	tokenHash string
}

type friendFeedCall struct {
	request friendFeedRequest
	ctx     context.Context
	rows    []feedRow
}

type friendFeedBatch struct {
	calls  []*friendFeedCall
	live   int
	done   chan struct{}
	cancel context.CancelFunc
	err    error
}

// A queue belongs to one server region and database. Only requests already
// waiting when we take the connection can share the resulting read snapshot.
type friendFeedQueue struct {
	mu      sync.Mutex
	pending *friendFeedBatch
}

func selectSingleFriendFeed(ctx context.Context, db sqlx.QueryerContext, region string, req friendFeedRequest) ([]feedRow, error) {
	var rows []feedRow
	err := sqlx.SelectContext(ctx, db, &rows, feedDeferredQueries["friends"], req.userID, nil, "", region, req.tokenHash)
	return rows, err
}

func (q *friendFeedQueue) selectRows(ctx context.Context, db *sqlx.DB, region string, req friendFeedRequest) ([]feedRow, error) {
	stats := db.Stats()
	if stats.Idle > 0 || stats.MaxOpenConnections == 0 || stats.OpenConnections < stats.MaxOpenConnections {
		return selectSingleFriendFeed(ctx, db, region, req)
	}

	call := &friendFeedCall{request: req, ctx: ctx}
	q.mu.Lock()
	b := q.pending
	if b == nil {
		shared, cancel := context.WithCancel(context.WithoutCancel(ctx))
		b = &friendFeedBatch{done: make(chan struct{}), cancel: cancel}
		q.pending = b
		go q.run(shared, db, region, b)
	}
	b.calls = append(b.calls, call)
	b.live++
	if len(b.calls) == maxFriendFeedBatch {
		q.pending = nil
	}
	q.mu.Unlock()

	select {
	case <-b.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return call.rows, b.err
	case <-ctx.Done():
		q.mu.Lock()
		b.live--
		if b.live == 0 {
			if q.pending == b {
				q.pending = nil
			}
			b.cancel()
		}
		q.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (q *friendFeedQueue) run(ctx context.Context, db *sqlx.DB, region string, b *friendFeedBatch) {
	defer close(b.done)
	defer b.cancel()
	conn, err := db.Connx(ctx)
	q.mu.Lock()
	if q.pending == b {
		q.pending = nil
	}
	q.mu.Unlock()
	if err != nil {
		b.err = err
		return
	}
	defer conn.Close()

	calls := b.calls[:0]
	var requests []friendFeedRequest
	for _, c := range b.calls {
		if c.ctx.Err() == nil {
			calls = append(calls, c)
			requests = append(requests, c.request)
		}
	}
	switch len(calls) {
	case 0:
	case 1:
		c := calls[0]
		c.rows, b.err = selectSingleFriendFeed(c.ctx, conn, region, c.request)
	default:
		var pages [][]feedRow
		pages, b.err = selectFriendFeedBatch(ctx, conn, region, requests)
		if b.err == nil {
			for i, page := range pages {
				calls[i].rows = page
			}
		}
	}
}
