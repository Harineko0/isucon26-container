package isureal

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

const diskImageBatchKey = "disk_image_metadata"

type leafImageReads struct {
	rel     string
	indices []int
	ids     []string
	hasData []bool
}

// Preserve each leaf's prepared SQL and partition pruning, but send it with
// the other leaves' reads on one checkout. Only metadata can enter this path.
func readMultipleImageLeaves(ctx context.Context, conn *sqlx.Conn, requests []imageReadRequest, indices []int, results []imageReadResult) {
	groups := make(map[string]*leafImageReads)
	var batch pgx.Batch
	for _, idx := range indices {
		req := requests[idx]
		group := groups[req.rel]
		if group == nil {
			group = &leafImageReads{rel: req.rel}
			groups[req.rel] = group
		}
		group.indices = append(group.indices, idx)
		group.ids = append(group.ids, req.id)
		group.hasData = append(group.hasData, true)
	}
	for _, group := range groups {
		batch.Queue(imageBatchQueryFor(group.rel), group.ids, group.hasData).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var ord int
				var object imageObject
				err := rows.Scan(&ord, &object.SecretKeyHash, &object.MediaType, &object.CreatedAt, &object.Partition, &object.Data, &object.StoredRegion, &object.Allowed)
				if err != nil {
					return err
				}
				if ord < 1 || ord > len(group.indices) {
					return fmt.Errorf("invalid image read index %d", ord)
				}
				results[group.indices[ord-1]] = imageReadResult{object: object}
			}
			return rows.Err()
		})
	}
	imageReadStats.Add("queries", int64(len(groups)-1))
	imageReadStats.Add("mixed_leaf_batches", 1)
	imageReadStats.Add("mixed_leaf_statements", int64(len(groups)))
	err := conn.Raw(func(raw any) error {
		pg, ok := raw.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("image pipeline requires pgx, got %T", raw)
		}
		return pg.Conn().SendBatch(ctx, &batch).Close()
	})
	if err == nil {
		return
	}
	// A leaf-specific SQL error must not turn an unrelated valid image into
	// a failure. These statements only read; redo them individually in that case.
	var stmtErr *pgconn.PgError
	if ctx.Err() == nil && errors.As(err, &stmtErr) {
		for _, idx := range indices {
			var obj imageObject
			req := requests[idx]
			e := conn.GetContext(ctx, &obj, imageQueryFor(req.rel), req.id, true)
			results[idx] = imageReadResult{object: obj, err: e}
		}
		return
	}
	for _, idx := range indices {
		results[idx] = imageReadResult{err: err}
	}
}
