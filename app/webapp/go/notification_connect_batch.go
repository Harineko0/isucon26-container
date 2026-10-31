package isureal

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/jmoiron/sqlx"
)

const notificationConnectSQL = `
 SELECT true AS found, u.region,
        (SELECT starts_at FROM posting_windows WHERE region = $3 AND ended_at IS NULL) AS active_window_starts_at
 FROM login_sessions s JOIN users u ON u.id = s.user_id
 WHERE s.token_hash = $1 AND s.active AND u.status = 'active' AND u.id = $2
`

// All subscribers are registered with the watcher before they join this read.
// Their session checks and active-window cutoff use one statement snapshot.
const notificationConnectBatchSQL = `
 SELECT r.idx, u.region,
        (SELECT starts_at FROM posting_windows WHERE region = $3 AND ended_at IS NULL)
 FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS r(token_hash, user_id, idx)
 JOIN login_sessions s ON s.token_hash = r.token_hash AND s.user_id = r.user_id AND s.active
 JOIN users u ON u.id = s.user_id AND u.status = 'active'
`

const maxNotificationConnectBatch = 64

type notificationConnectQueue struct {
	db     *sqlx.DB
	region string

	mu      sync.Mutex
	pending *notificationConnectBatch
}

type notificationConnectCall struct {
	tokenHash string
	userID    string
	state     notificationConnectState
}

type notificationConnectBatch struct {
	calls  []notificationConnectCall
	done   chan struct{}
	err    error
	live   int
	cancel context.CancelFunc
}

func readNotificationConnect(ctx context.Context, db sqlx.QueryerContext, region, tokenHash, userID string) (notificationConnectState, error) {
	var state notificationConnectState
	err := sqlx.GetContext(ctx, db, &state, notificationConnectSQL, tokenHash, userID, region)
	if errors.Is(err, sql.ErrNoRows) {
		return notificationConnectState{}, nil
	}
	return state, err
}

func (q *notificationConnectQueue) get(ctx context.Context, tokenHash, userID string) (notificationConnectState, error) {
	if err := ctx.Err(); err != nil {
		return notificationConnectState{}, err
	}
	stats := q.db.Stats()
	if stats.Idle > 0 || stats.MaxOpenConnections == 0 || stats.OpenConnections < stats.MaxOpenConnections {
		return readNotificationConnect(ctx, q.db, q.region, tokenHash, userID)
	}

	q.mu.Lock()
	b := q.pending
	if b == nil {
		shared, cancel := context.WithCancel(context.WithoutCancel(ctx))
		b = &notificationConnectBatch{done: make(chan struct{}), cancel: cancel}
		q.pending = b
		go q.run(shared, b)
	}
	idx := len(b.calls)
	b.calls = append(b.calls, notificationConnectCall{tokenHash: tokenHash, userID: userID})
	b.live++
	if len(b.calls) == maxNotificationConnectBatch {
		q.pending = nil
	}
	q.mu.Unlock()

	select {
	case <-b.done:
		return b.calls[idx].state, b.err
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
		return notificationConnectState{}, ctx.Err()
	}
}

func (q *notificationConnectQueue) run(ctx context.Context, b *notificationConnectBatch) {
	defer b.cancel()
	defer close(b.done)
	conn, err := q.db.Connx(ctx)
	q.mu.Lock()
	// Freeze admission before executing SQL. A subscriber arriving after a
	// committed logout or window change needs a fresh snapshot, even when
	// the previous batch is still reading its results.
	if q.pending == b {
		q.pending = nil
	}
	q.mu.Unlock()
	if err != nil {
		b.err = err
		return
	}
	defer conn.Close()
	if len(b.calls) == 1 {
		c := &b.calls[0]
		c.state, b.err = readNotificationConnect(ctx, conn, q.region, c.tokenHash, c.userID)
		return
	}
	tokens := make([]string, len(b.calls))
	users := make([]string, len(b.calls))
	for i, c := range b.calls {
		tokens[i], users[i] = c.tokenHash, c.userID
	}
	rows, err := conn.QueryContext(ctx, notificationConnectBatchSQL, tokens, users, q.region)
	if err != nil {
		b.err = err
		return
	}
	defer rows.Close()
	for rows.Next() {
		var idx int
		state := notificationConnectState{Found: true}
		if err := rows.Scan(&idx, &state.Region, &state.ActiveWindowStartsAt); err != nil {
			b.err = err
			return
		}
		b.calls[idx-1].state = state
	}
	b.err = rows.Err()
}
