package isureal

import (
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type getFeedRequest struct {
	Feed           string     `param:"feed"`
	BeforeScore    *int64     `query:"beforeScore"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type feedItemResponse struct {
	createPostResponse
	Score *int64 `json:"score,omitempty"`
}

type getFeedResponse struct {
	Items   []feedItemResponse `json:"items"`
	HasMore bool               `json:"hasMore"`
}

func getFeedHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getFeedRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		switch input.Feed {
		case "friends", "friends-of-friends", "public", "following":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if (input.BeforePostedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ranked := input.Feed == "friends-of-friends"
		if ranked && (input.BeforeScore == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		// 閲覧可否・フィードの種類・おすすめ度・ページングをすべて DB 側で計算し、51 件だけを受け取る。
		var posts []feedRow
		if err := sqlx.SelectContext(ctx, h.DB, &posts, feedQuery,
			GetSession(c).UserID, input.Feed, input.BeforePostedAt, input.BeforeID, input.BeforeScore); err != nil {
			return err
		}
		plan := newSignaturePlan()
		for i := range posts {
			if err := posts[i].Author.setImageKey(); err != nil {
				return err
			}
			plan.add(posts[i].Author, posts[i].ImageID)
			plan.addProfile(posts[i].Author)
		}
		plan.run()
		items := make([]feedItemResponse, 0, len(posts))
		for _, post := range posts {
			item := feedItemResponse{createPostResponse: newPostResponse(post.Post, post.Author)}
			if ranked {
				score := post.Score
				item.Score = &score
			}
			items = append(items, item)
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getFeedResponse{Items: items, HasMore: hasMore})
	}
}

type feedRow struct {
	PostWithAuthor
	Score int64 `db:"score"`
}

// feedQuery は viewerContext.canView・matchesFeed・recommendationScores と同じ判定を SQL で行う。
// $1 閲覧者, $2 フィード, $3 beforePostedAt, $4 beforeId, $5 beforeScore (friends-of-friends のみ)
var feedQuery = `
  WITH viewer AS (
    SELECT EXISTS(
      SELECT 1 FROM posts p
      JOIN posting_windows posted ON posted.id = p.posting_window_id
      JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                  AND current.local_date = posted.local_date
      WHERE p.author_id = u.id AND p.deleted_at IS NULL
    ) AS has_posted_today
    FROM users u WHERE u.id = $1 AND u.status = 'active'
  ),
  friends AS (
    SELECT CASE WHEN user_low_id = $1 THEN user_high_id ELSE user_low_id END AS id
    FROM friendships WHERE user_low_id = $1 OR user_high_id = $1
  ),
  blocked AS (
    SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END AS id
    FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
  ),
  mutual AS (
    SELECT id, count(*) AS n FROM (
      SELECT fs.user_high_id AS id FROM friendships fs JOIN friends f ON fs.user_low_id = f.id
      UNION ALL
      SELECT fs.user_low_id AS id FROM friendships fs JOIN friends f ON fs.user_high_id = f.id
    ) x WHERE id <> $1 GROUP BY id
  ),
  candidates AS (
    SELECT ` + postColumns + `, ` + userColumnsAs("u", "author") + `
    FROM posts p
    JOIN posting_windows w ON w.id = p.posting_window_id AND w.ended_at IS NULL
    JOIN users u ON u.id = p.author_id AND u.status = 'active'
    WHERE p.deleted_at IS NULL AND p.author_id <> $1
      AND COALESCE((SELECT has_posted_today FROM viewer), false)
      AND p.author_id NOT IN (SELECT id FROM blocked)
      AND (p.visibility = 'public'
           OR (p.visibility = 'friends' AND p.author_id IN (SELECT id FROM friends))
           OR (p.visibility = 'friends_of_friends' AND (p.author_id IN (SELECT id FROM friends) OR p.author_id IN (SELECT id FROM mutual))))
      AND CASE $2
        WHEN 'public' THEN p.visibility = 'public'
        WHEN 'following' THEN p.author_id IN (SELECT followee_id FROM follows WHERE follower_id = $1)
        WHEN 'friends' THEN p.author_id IN (SELECT id FROM friends)
        WHEN 'friends-of-friends' THEN p.author_id NOT IN (SELECT id FROM friends) AND p.author_id IN (SELECT id FROM mutual)
        ELSE false END
  ),
  scored AS (
    SELECT c.*, CASE WHEN $2 = 'friends-of-friends' THEN
      10 * (SELECT count(*) FROM (
              SELECT r.reactor_id AS user_id FROM realmojis r JOIN users ru ON ru.id = r.reactor_id AND ru.status = 'active'
              WHERE r.post_id = c.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
              UNION
              SELECT cm.author_id FROM comments cm JOIN users cu ON cu.id = cm.author_id AND cu.status = 'active'
              WHERE cm.post_id = c.id AND cm.deleted_at IS NULL
            ) e WHERE e.user_id IN (SELECT id FROM friends))
      + 3 * COALESCE((SELECT n FROM mutual WHERE id = c.author_id), 0)
      + LEAST(COALESCE((SELECT sum(m.n) FROM friendships fs
                        JOIN mutual m ON m.id = CASE WHEN fs.user_low_id = c.author_id THEN fs.user_high_id ELSE fs.user_low_id END
                        WHERE fs.user_low_id = c.author_id OR fs.user_high_id = c.author_id), 0), 30)
      ELSE 0 END::bigint AS score
    FROM candidates c
  )
  SELECT * FROM scored
  WHERE CASE WHEN $2 = 'friends-of-friends'
    THEN $5::bigint IS NULL OR (score, posted_at, id) < ($5, $3::timestamptz, $4)
    ELSE $3::timestamptz IS NULL OR (posted_at, id) < ($3, $4) END
  ORDER BY score DESC, posted_at DESC, id DESC
  LIMIT 51
`
