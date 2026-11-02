package isureal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
)

// Enter each existing account function in username order to take its locks
// consistently across batches. Calls for the same username retain arrival order.
// Return rows in request order, only after the statement's implicit commit.
const loginBatchSQL = `
 WITH input AS MATERIALIZED (
   SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[])
     WITH ORDINALITY AS q(username, secret, session_id, token_hash, ord)
   ORDER BY username COLLATE "C", ord
 )
 SELECT r.* FROM input i CROSS JOIN LATERAL
   isureal_login(i.username, i.secret, i.session_id, i.token_hash) r
 ORDER BY i.ord`

const registerBatchSQL = `
 WITH input AS MATERIALIZED (
   SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[])
     WITH ORDINALITY AS q(user_id, username, secret, display_name, region, ord)
   ORDER BY username COLLATE "C", ord
 )
 SELECT r.* FROM input i CROSS JOIN LATERAL
   isureal_register(i.user_id, i.username, i.secret, i.display_name, i.region) r
 ORDER BY i.ord`

func (q *writeQueue[T]) batchAccounts(ctx context.Context, conn *sqlx.Conn, calls []*writeCall[T]) bool {
	if q.accountBatchSQL == "" {
		return false
	}
	columns := make([]any, len(calls[0].args))
	for j := range columns {
		column := make([]string, len(calls))
		for i, call := range calls {
			if len(call.args) != len(columns) {
				return false
			}
			val, ok := call.args[j].(string)
			if !ok {
				return false
			}
			column[i] = val
		}
		columns[j] = column
	}
	var rows []T
	err := conn.SelectContext(ctx, &rows, q.accountBatchSQL, columns...)
	var sqlErr *pgconn.PgError
	if errors.As(err, &sqlErr) && (strings.HasPrefix(sqlErr.Code, "23") || sqlErr.Code == "40P01" || sqlErr.Code == "40001") {
		// The server explicitly rolled back all writes. Isolate the requests
		// to preserve independent failures. Never retry a lost response.
		return false
	}
	if err == nil && len(rows) != len(calls) {
		err = fmt.Errorf("account batch returned %d rows for %d requests", len(rows), len(calls))
	}
	for i, call := range calls {
		result := writeResult[T]{err: err}
		if err == nil {
			result.row = rows[i]
		}
		call.reply <- result
	}
	return true
}
