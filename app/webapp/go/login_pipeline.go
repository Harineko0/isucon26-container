package isureal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

const loginSQL = `SELECT * FROM isureal_login($1, $2, $3, $4)`
const maxPipelinedWrites = 8

type writeCall[T any] struct {
	ctx   context.Context
	ip    string
	args  [4]string
	reply chan writeResult[T]
}

type writeResult[T any] struct {
	row T
	err error
}

type writeBatch[T any] struct {
	calls  []*writeCall[T]
	live   int
	cancel context.CancelFunc
}

type writeQueue[T any] struct {
	db      *sqlx.DB
	bans    *IPBanList // Only login rechecks IP bans after acquiring its connection.
	sql     string
	mu      sync.Mutex
	pending *writeBatch[T]
}

func (q *writeQueue[T]) query(ctx context.Context, conn *sqlx.Conn, args [4]string) (T, error) {
	var row T
	err := conn.GetContext(ctx, &row, q.sql, args[0], args[1], args[2], args[3])
	return row, err
}

// Use the regular prepared query while the pool has headroom. Under pressure
// only arrivals overlapping acquisition share a connection, without a timer.
func (q *writeQueue[T]) execute(ctx context.Context, ip string, args [4]string) (T, error) {
	var zero T
	stats := q.db.Stats()
	if stats.Idle > 0 || stats.MaxOpenConnections == 0 || stats.OpenConnections < stats.MaxOpenConnections {
		conn, err := q.db.Connx(ctx)
		if err != nil {
			return zero, err
		}
		defer conn.Close()
		if q.bans != nil && q.bans.bannedCached(ip, time.Now()) {
			return zero, NewAPIError(http.StatusForbidden, "ip_banned")
		}
		return q.query(ctx, conn, args)
	}
	call := &writeCall[T]{ctx: ctx, ip: ip, args: args, reply: make(chan writeResult[T], 1)}
	q.mu.Lock()
	b := q.pending
	if b == nil {
		shared, cancel := context.WithCancel(context.WithoutCancel(ctx))
		b = &writeBatch[T]{cancel: cancel}
		q.pending = b
		go q.run(shared, b)
	}
	b.calls = append(b.calls, call)
	b.live++
	if len(b.calls) == maxPipelinedWrites {
		q.pending = nil
	}
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		b.live--
		if b.live == 0 && ctx.Err() != nil {
			if q.pending == b {
				q.pending = nil
			}
			b.cancel()
		}
		q.mu.Unlock()
	}()

	select {
	case result := <-call.reply:
		return result.row, result.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func (q *writeQueue[T]) run(ctx context.Context, b *writeBatch[T]) {
	defer b.cancel()
	conn, err := q.db.Connx(ctx)
	q.mu.Lock()
	if q.pending == b {
		q.pending = nil
	}
	calls := b.calls
	q.mu.Unlock()
	if err != nil {
		for _, c := range calls {
			c.reply <- writeResult[T]{err: err}
		}
		return
	}
	defer conn.Close()

	// A ban or a cancellation can have arrived while the pool was full.
	ready := calls[:0]
	for _, c := range calls {
		switch {
		case c.ctx.Err() != nil:
			c.reply <- writeResult[T]{err: c.ctx.Err()}
		case q.bans != nil && q.bans.bannedCached(c.ip, time.Now()):
			c.reply <- writeResult[T]{err: NewAPIError(http.StatusForbidden, "ip_banned")}
		default:
			ready = append(ready, c)
		}
	}
	switch len(ready) {
	case 0:
	case 1:
		c := ready[0]
		row, err := q.query(c.ctx, conn, c.args)
		c.reply <- writeResult[T]{row, err}
	default:
		q.pipeline(ctx, conn, ready)
	}
}

func (q *writeQueue[T]) pipeline(ctx context.Context, conn *sqlx.Conn, calls []*writeCall[T]) {
	err := conn.Raw(func(raw any) error {
		driverConn, ok := raw.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("write pipeline requires pgx, got %T", raw)
		}
		pg := driverConn.Conn()
		pipe := pg.PgConn().StartPipeline(ctx)
		defer pipe.Close()
		for _, c := range calls {
			args := [][]byte{[]byte(c.args[0]), []byte(c.args[1]), []byte(c.args[2]), []byte(c.args[3])}
			pipe.SendQueryParams(q.sql, args, nil, nil, nil)
			// Sync ends each transaction, so locks and statement failures stay
			// within one request. Flush the whole batch to cross the network once.
			pipe.SendPipelineSync()
		}
		if err := pipe.Flush(); err != nil {
			return err
		}
		for len(calls) > 0 {
			result, err := readWriteResult[T](pg, pipe)
			if err != nil {
				return err
			}
			calls[0].reply <- result
			calls = calls[1:]
		}
		return nil
	})
	// A lost connection leaves the commit outcome unknown. Never replay writes.
	for _, c := range calls {
		c.reply <- writeResult[T]{err: err}
	}
}

func readWriteResult[T any](pg *pgx.Conn, pipe *pgconn.Pipeline) (writeResult[T], error) {
	result := writeResult[T]{err: pgx.ErrNoRows}
	for {
		msg, err := pipe.GetResults()
		if err != nil {
			var statementError *pgconn.PgError
			if !errors.As(err, &statementError) {
				return writeResult[T]{}, err
			}
			result.err = err
			continue // A statement or commit error is followed by ReadyForQuery.
		}
		switch msg := msg.(type) {
		case *pgconn.ResultReader:
			rows := pgx.RowsFromResultReader(pg.TypeMap(), msg)
			result.row, result.err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[T])
		case *pgconn.PipelineSync:
			return result, nil // The server has committed or rolled back this request.
		default:
			return writeResult[T]{}, fmt.Errorf("unexpected write pipeline response %T", msg)
		}
	}
}
