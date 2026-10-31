package isureal

import (
	"context"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
)

// These are immutable facts about JPEGs created by isureal_create_post. Only
// a successful commit populates the map. Capture its generation before sending
// the write so an in-flight create cannot refill a discarded generation.
type postImageProof struct {
	postID, ownerID string
}

var committedPostImages atomic.Pointer[boundedMap]

func init() { resetCommittedPostImages() }

func resetCommittedPostImages() {
	committedPostImages.Store(&boundedMap{limit: 32_768})
}

// This key is separate from all image_objects partition names. Concurrent
// requests for recent posts can share one fresh snapshot across partitions.
const postImageBatchKey = "committed_post_images"

func loadCommittedPostImages(ctx context.Context, conn *sqlx.Conn, requests []imageReadRequest, indices []int, results []imageReadResult) {
	postIDs := make([]string, len(indices))
	for i, index := range indices {
		postIDs[i] = requests[index].post.postID
	}
	var rows []struct {
		Index         int    `db:"request_index"`
		ImageID       string `db:"image_id"`
		OwnerID       string `db:"author_id"`
		SecretKeyHash string `db:"secret_key_hash"`
	}
	// Re-read the association, author status, and deletion marker even for
	// local bytes. initialize removes new posts together with their objects.
	err := conn.SelectContext(ctx, &rows, `
		SELECT q.ordinality AS request_index, p.image_id, p.author_id, u.secret_key_hash
		FROM unnest($1::text[]) WITH ORDINALITY q(post_id, ordinality)
		JOIN posts p ON p.id = q.post_id AND p.deleted_at IS NULL
		JOIN users u ON u.id = p.author_id AND u.status = 'active'
	`, postIDs)
	if err != nil {
		for _, index := range indices {
			results[index].err = err
		}
		return
	}
	for _, row := range rows {
		index := indices[row.Index-1]
		request := requests[index]
		if row.ImageID == request.id && row.OwnerID == request.post.ownerID {
			results[index] = imageReadResult{object: imageObject{
				SecretKeyHash: row.SecretKeyHash, MediaType: "image/jpeg", Allowed: true,
			}}
		}
	}
}
