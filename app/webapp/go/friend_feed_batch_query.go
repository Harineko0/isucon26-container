package isureal

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// Each input keeps its own session and visibility checks. Only the scan of
// the open windows' posts is shared. Fetch the full posts after the per-input
// limit so captions and coordinates are not copied into the shared scan.
var friendFeedBatchQuery = `
WITH viewers AS MATERIALIZED (
    SELECT q.request_index, u.id AS viewer_id, u.region AS sess_region,
        (u.region = $3 AND EXISTS (
            SELECT 1 FROM posts p
            JOIN posting_windows posted ON posted.id = p.posting_window_id
            JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                        AND current.local_date = posted.local_date
            WHERE p.author_id = u.id AND p.deleted_at IS NULL
        )) AS eligible
    FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS q(viewer_id, token_hash, request_index)
    JOIN login_sessions s ON s.token_hash = q.token_hash AND s.active AND s.user_id = q.viewer_id
    JOIN users u ON u.id = s.user_id AND u.status = 'active'
), friends AS (
    SELECT v.request_index, f.author_id
    FROM viewers v
    CROSS JOIN LATERAL (
        SELECT fs.user_high_id AS author_id FROM friendships fs WHERE fs.user_low_id = v.viewer_id
        UNION
        SELECT fs.user_low_id FROM friendships fs WHERE fs.user_high_id = v.viewer_id
    ) f
    WHERE v.eligible AND f.author_id <> v.viewer_id
      AND NOT EXISTS (SELECT 1 FROM block_relations WHERE blocker_id = v.viewer_id AND blocked_id = f.author_id)
      AND NOT EXISTS (SELECT 1 FROM block_relations WHERE blocker_id = f.author_id AND blocked_id = v.viewer_id)
), open_posts AS MATERIALIZED (
    SELECT p.id, p.author_id, p.posted_at
    FROM posting_windows w JOIN posts p ON p.posting_window_id = w.id
    WHERE w.ended_at IS NULL AND p.deleted_at IS NULL
      AND p.visibility IN ('public', 'friends', 'friends_of_friends')
      AND EXISTS (SELECT 1 FROM viewers WHERE eligible)
), ranked AS (
    SELECT f.request_index, p.id,
        row_number() OVER (PARTITION BY f.request_index ORDER BY p.posted_at DESC, p.id DESC) AS position
    FROM open_posts p JOIN friends f ON f.author_id = p.author_id
    WHERE EXISTS (SELECT 1 FROM users u WHERE u.id = p.author_id AND u.status = 'active')
)
SELECT v.request_index, v.sess_region, r.id IS NOT NULL AS found,
    ` + postWithAuthorColumnsCoalesced("r") + `, 0::bigint AS score
FROM viewers v LEFT JOIN (
    SELECT k.request_index, ` + postColumns + `, ` + userColumnsAs("u", "author") + `
    FROM ranked k JOIN posts p ON p.id = k.id
    JOIN posting_windows w ON w.id = p.posting_window_id
    JOIN users u ON u.id = p.author_id
    WHERE k.position <= 51
) r ON r.request_index = v.request_index
ORDER BY v.request_index, r.posted_at DESC, r.id DESC`

func selectFriendFeedBatch(ctx context.Context, conn *sqlx.Conn, region string, calls []friendFeedRequest) ([][]feedRow, error) {
	userIDs := make([]string, len(calls))
	tokens := make([]string, len(calls))
	for i, call := range calls {
		userIDs[i] = call.userID
		tokens[i] = call.tokenHash
	}
	var rows []struct {
		Index int `db:"request_index"`
		feedRow
	}
	if err := conn.SelectContext(ctx, &rows, friendFeedBatchQuery, userIDs, tokens, region); err != nil {
		return nil, err
	}
	results := make([][]feedRow, len(calls))
	for _, row := range rows {
		index := row.Index - 1 // WITH ORDINALITY starts at 1.
		results[index] = append(results[index], row.feedRow)
	}
	return results, nil
}
