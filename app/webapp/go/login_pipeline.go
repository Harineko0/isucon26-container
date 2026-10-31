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
const maxPipelinedLogins = 8

type loginCall struct {
	ctx   context.Context
	ip    string
	args  [4]string
	reply chan loginResult
}

type loginResult struct {
	row sessionUserRow
	err error
}

type loginBatch struct {
	calls  []*loginCall
	live   int
	cancel context.CancelFunc
}

type loginQueue struct {
	h       *HandlerContext
	mu      sync.Mutex
	pending *loginBatch
}

func queryLogin(ctx context.Context, conn *sqlx.Conn, args [4]string) (sessionUserRow, error) {
	var row sessionUserRow
	err := conn.GetContext(ctx, &row, loginSQL, args[0], args[1], args[2], args[3])
	return row, err
}

// Use the regular prepared query while the pool has headroom. Under pressure
// only arrivals overlapping acquisition share a connection, without a timer.
func (q *loginQueue) login(ctx context.Context, ip string, args [4]string) (sessionUserRow, error) {
	stats := q.h.DB.Stats()
	if stats.Idle > 0 || stats.MaxOpenConnections == 0 || stats.OpenConnections < stats.MaxOpenConnections {
		conn, err := q.h.IPBans.connUnlessBanned(ctx, q.h.DB, ip)
		if err != nil {
			return sessionUserRow{}, err
		}
		defer conn.Close()
		return queryLogin(ctx, conn, args)
	}
	call := &loginCall{ctx: ctx, ip: ip, args: args, reply: make(chan loginResult, 1)}
	q.mu.Lock()
	b := q.pending
	if b == nil {
		shared, cancel := context.WithCancel(context.WithoutCancel(ctx))
		b = &loginBatch{cancel: cancel}
		q.pending = b
		go q.run(shared, b)
	}
	b.calls = append(b.calls, call)
	b.live++
	if len(b.calls) == maxPipelinedLogins {
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
		return sessionUserRow{}, ctx.Err()
	}
}

func (q *loginQueue) run(ctx context.Context, b *loginBatch) {
	defer b.cancel()
	conn, err := q.h.DB.Connx(ctx)
	q.mu.Lock()
	if q.pending == b {
		q.pending = nil
	}
	calls := b.calls
	q.mu.Unlock()
	if err != nil {
		for _, c := range calls {
			c.reply <- loginResult{err: err}
		}
		return
	}
	defer conn.Close()

	// A ban or a cancellation can have arrived while the pool was full.
	ready := calls[:0]
	for _, c := range calls {
		switch {
		case c.ctx.Err() != nil:
			c.reply <- loginResult{err: c.ctx.Err()}
		case q.h.IPBans.bannedCached(c.ip, time.Now()):
			c.reply <- loginResult{err: NewAPIError(http.StatusForbidden, "ip_banned")}
		default:
			ready = append(ready, c)
		}
	}
	switch len(ready) {
	case 0:
	case 1:
		c := ready[0]
		row, err := queryLogin(c.ctx, conn, c.args)
		c.reply <- loginResult{row, err}
	default:
		pipelineLogins(ctx, conn, ready)
	}
}

func pipelineLogins(ctx context.Context, conn *sqlx.Conn, calls []*loginCall) {
	err := conn.Raw(func(raw any) error {
		driverConn, ok := raw.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("login pipeline requires pgx, got %T", raw)
		}
		pg := driverConn.Conn()
		pipe := pg.PgConn().StartPipeline(ctx)
		defer pipe.Close()
		for _, c := range calls {
			args := [][]byte{[]byte(c.args[0]), []byte(c.args[1]), []byte(c.args[2]), []byte(c.args[3])}
			pipe.SendQueryParams(loginSQL, args, nil, nil, nil)
			// Sync ends each transaction, so locks and statement failures stay
			// within one login. Flush the whole batch to cross the network once.
			pipe.SendPipelineSync()
		}
		if err := pipe.Flush(); err != nil {
			return err
		}
		for len(calls) > 0 {
			result, err := readLoginResult(pg, pipe)
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
		c.reply <- loginResult{err: err}
	}
}

func readLoginResult(pg *pgx.Conn, pipe *pgconn.Pipeline) (loginResult, error) {
	result := loginResult{err: pgx.ErrNoRows}
	for {
		msg, err := pipe.GetResults()
		if err != nil {
			var statementError *pgconn.PgError
			if !errors.As(err, &statementError) {
				return loginResult{}, err
			}
			result.err = err
			continue // A statement or commit error is followed by ReadyForQuery.
		}
		switch msg := msg.(type) {
		case *pgconn.ResultReader:
			rows := pgx.RowsFromResultReader(pg.TypeMap(), msg)
			result.row, result.err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[sessionUserRow])
		case *pgconn.PipelineSync:
			return result, nil // The server has committed or rolled back this login.
		default:
			return loginResult{}, fmt.Errorf("unexpected login pipeline response %T", msg)
		}
	}
}
