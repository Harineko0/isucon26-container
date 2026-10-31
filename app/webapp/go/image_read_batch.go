package isureal

import (
	"context"
	"database/sql"
	"expvar"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type imageObject struct {
	SecretKeyHash string    `db:"secret_key_hash"`
	MediaType     string    `db:"media_type"`
	Data          []byte    `db:"data"`
	StoredRegion  *string   `db:"stored_region"`
	Allowed       bool      `db:"allowed"`
	CreatedAt     time.Time `db:"created_at"`
	Partition     string    `db:"partition"`
}

type imageReadRequest struct {
	id      string
	hasData bool
	ip      string
}

type imageReadResult struct {
	object imageObject
	err    error
}

// Only requests received before the SELECT starts may share its snapshot.
// A request after a completed deletion must never join an older running read.
type imageReads struct {
	mu      sync.Mutex
	pending map[string]*imageReadBatch
	load    func(context.Context, string, []imageReadRequest) []imageReadResult
}

type imageReadBatch struct {
	requests []imageReadRequest
	waiters  int
	full     chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
	results  []imageReadResult
}

const maxImageReadBatch = 64

var imageReadStats = expvar.NewMap("image_read_batches")
var imageBatchQueries sync.Map

func (r *imageReads) get(ctx context.Context, rel string, request imageReadRequest) (imageObject, error) {
	if err := ctx.Err(); err != nil {
		return imageObject{}, err
	}
	r.mu.Lock()
	b := r.pending[rel]
	if b == nil {
		queryCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		b = &imageReadBatch{full: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
		if r.pending == nil {
			r.pending = make(map[string]*imageReadBatch)
		}
		r.pending[rel] = b
		go r.read(queryCtx, rel, b)
	}
	index := len(b.requests)
	b.requests = append(b.requests, request)
	b.waiters++
	if len(b.requests) == maxImageReadBatch {
		delete(r.pending, rel)
		close(b.full)
	}
	r.mu.Unlock()
	select {
	case <-b.done:
		return b.results[index].object, b.results[index].err
	case <-ctx.Done():
		r.mu.Lock()
		b.waiters--
		if b.waiters == 0 {
			if r.pending[rel] == b {
				delete(r.pending, rel)
			}
			b.cancel()
		}
		r.mu.Unlock()
		return imageObject{}, ctx.Err()
	}
}

func (r *imageReads) read(ctx context.Context, rel string, b *imageReadBatch) {
	defer b.cancel()
	defer close(b.done)
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-b.full:
	case <-ctx.Done():
	}
	r.mu.Lock()
	if r.pending[rel] == b {
		delete(r.pending, rel)
	}
	// Freeze the request set under the same lock used for admission.
	requests := b.requests
	r.mu.Unlock()
	b.results = r.load(ctx, rel, requests)
}

// Derive the batch SELECT from the single-image SELECT so its visibility and
// deletion rules cannot diverge. Only the input relation and parameters change.
func imageBatchQueryFor(rel string) string {
	if query, ok := imageBatchQueries.Load(rel); ok {
		return query.(string)
	}
	query := imageQueryFor(rel)
	query = strings.Replace(query, "SELECT u.secret_key_hash", "SELECT request.ordinality AS request_index, u.secret_key_hash", 1)
	query = strings.ReplaceAll(query, "$2", "request.has_data")
	query = strings.ReplaceAll(query, "$1", "request.id")
	query = strings.Replace(query, "FROM "+rel+" i JOIN users", `FROM unnest($1::text[], $2::boolean[]) WITH ORDINALITY AS request(id, has_data, ordinality)
     JOIN `+rel+` i ON i.id = request.id JOIN users`, 1)
	imageBatchQueries.Store(rel, query)
	return query
}

func loadImageReadBatch(ctx context.Context, h *HandlerContext, rel string, requests []imageReadRequest) []imageReadResult {
	results := make([]imageReadResult, len(requests))
	conn, err := h.DB.Connx(ctx)
	if err != nil {
		for i := range results {
			results[i].err = err
		}
		return results
	}
	defer conn.Close()
	ids := make([]string, 0, len(requests))
	hasData := make([]bool, 0, len(requests))
	indices := make([]int, 0, len(requests))
	for i, request := range requests {
		// Preserve connUnlessBanned's check after pool acquisition, for every IP.
		if h.IPBans != nil && h.IPBans.bannedCached(request.ip, time.Now()) {
			results[i].err = NewAPIError(http.StatusForbidden, "ip_banned")
			continue
		}
		ids = append(ids, request.id)
		hasData = append(hasData, request.hasData)
		indices = append(indices, i)
		results[i].err = sql.ErrNoRows
	}
	if len(ids) == 0 {
		return results
	}
	imageReadStats.Add("requests", int64(len(ids)))
	imageReadStats.Add("queries", 1)
	var rows []struct {
		Index int `db:"request_index"`
		imageObject
	}
	err = conn.SelectContext(ctx, &rows, imageBatchQueryFor(rel), ids, hasData)
	if err != nil {
		for _, i := range indices {
			results[i].err = err
		}
		return results
	}
	for _, row := range rows {
		if row.Index < 1 || row.Index > len(indices) {
			for _, i := range indices {
				results[i].err = fmt.Errorf("invalid image read request index: %d", row.Index)
			}
			return results
		}
		results[indices[row.Index-1]] = imageReadResult{object: row.imageObject}
	}
	return results
}

func readImageObject(ctx context.Context, h *HandlerContext, rel string, request imageReadRequest) (imageObject, error) {
	// Batch metadata only. A cache miss can return up to 32 MiB of bytea;
	// grouping those bodies would multiply the memory held by one DB connection.
	if h.imageReads != nil && request.hasData {
		return h.imageReads.get(ctx, rel, request)
	}
	// Keep standalone handlers usable without starting a background batcher.
	conn, err := h.IPBans.connUnlessBanned(ctx, h.DB, request.ip)
	if err != nil {
		return imageObject{}, err
	}
	defer conn.Close()
	var object imageObject
	err = conn.GetContext(ctx, &object, imageQueryFor(rel), request.id, request.hasData)
	return object, err
}
