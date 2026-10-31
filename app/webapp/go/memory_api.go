package isureal

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type getMemoriesRequest struct {
	Month          string     `query:"month"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type memoryPostResponse struct {
	createPostResponse
	RealMojis []replaceRealMojiResponse `json:"realmojis"`
	Comments  []createCommentResponse   `json:"comments"`
}

type getMemoriesResponse struct {
	Items   []memoryPostResponse `json:"items"`
	HasMore bool                 `json:"hasMore"`
}

type getMemoriesByDateRequest struct {
	Date           string     `param:"date"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type getMemoriesByDateResponse getMemoriesResponse

func getMemoriesHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse("2006-01", input.Month)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c, h, start, start.AddDate(0, 1, 0), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, response)
	}
}

func getMemoriesByDateHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesByDateRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse(time.DateOnly, input.Date)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c, h, start, start.AddDate(0, 0, 1), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMemoriesByDateResponse(response))
	}
}

// memoriesFrom は期間内の投稿期間ごとに、著者の投稿を posts(author_id, posting_window_id) で 1 件ずつ引く。
// 普通の JOIN だと、プランナは著者の全投稿 (最大 365 件、それぞれ別ページ) を読んでから期間で絞る
// (random_page_cost=1.1 ではヒープ読みを安く見積もるため)。OFFSET 0 で副問い合わせの平坦化を止め、
// 投稿期間ごとの索引引きに固定する。結果は通常の JOIN と同じ。検証: sql/tests/posts_author_window.sh
// memoriesQuery は memoryAnyOf でまとめて引くようになったので、いまは memory_api_test.go の旧実装 (比較の基準) と
// sql/tests/posts_author_window.sh だけが使う。
const memoriesFrom = `posting_windows w
  CROSS JOIN LATERAL (SELECT * FROM posts p WHERE p.author_id = $1 AND p.posting_window_id = w.id OFFSET 0) p`

// User の公開 JSON に含めない署名鍵と画像 ID は、DB からの内部転送でだけ別に受け取る。
type memoryInteraction struct {
	ID             string
	Body           string
	ImageID        string
	ReactionType   string
	CreatedAt      time.Time
	User           User
	SigningKey     string
	ProfileImageID *string
}

type memoryPageRow struct {
	SessRegion *string `db:"sess_region"`
	Found      bool    `db:"found"`
	HasMore    bool    `db:"has_more"`
	PostWithAuthor
	ReactionsJSON []byte `db:"reactions"`
	CommentsJSON  []byte `db:"comments"`
}

const memoryInteractionUser = `json_build_object(
  'id', u.id, 'username', u.username, 'displayName', u.display_name, 'bio', u.bio,
  'region', u.region, 'accountType', u.account_type, 'status', u.status) AS "User",
  u.secret_key_hash AS "SigningKey", u.profile_image_id AS "ProfileImageID"`

// memoryAnyOf は `col = ANY(ARRAY(SELECT id FROM src))` を、空の配列との OR にしたもの。
// 本番の DB 時間の 4 分の 3 はヒープのディスク読み (1 回 ~3ms) で (#154 の pg_stat_statements: blk_read_time
// 111.6s / total 147.3s)、索引スキャンはヒープを 1 ページずつ同期で読む。OR にすると索引の引き方は
// BitmapOr → Bitmap Heap Scan だけになり、effective_io_concurrency でヒープを先読みする。
// 空の側は索引を 1 度も降りない。条件の意味は `col = ANY(ARRAY(SELECT id FROM src))` と同じ。
func memoryAnyOf(column, src string) string {
	return `(` + column + ` = ANY(ARRAY(SELECT id FROM ` + src + `)) OR ` + column + ` = ANY(ARRAY(SELECT id FROM ` + src + ` WHERE false)))`
}

// 投稿ページを一度だけ取得し、その50件のリアクションも同じスナップショットで読む。
// 空ページでもセッションの行を残し、401/403 と空の200を区別する。
// 投稿・リアクション・コメントはそれぞれ 1 回の Bitmap Heap Scan でまとめて引き (memoryAnyOf)、
// 投稿ごとの並べ替えと 50 件の上限は、引いた行 (rx, cx) に対して以前と同じ式で行う。
var memoriesQuery = `
  WITH sess AS MATERIALIZED (SELECT * FROM {session}),
  wins AS MATERIALIZED (
    SELECT w.id, w.on_time_deadline, w.ended_at FROM posting_windows w WHERE w.local_date >= $2 AND w.local_date < $3
  ),
  page AS MATERIALIZED (
    SELECT ` + postColumns + `, ` + userColumnsAs("u", "author") + `
    FROM posts p
    JOIN wins w ON w.id = p.posting_window_id
    JOIN users u ON u.id = p.author_id AND u.status = 'active'
    JOIN sess s ON s.sess_user_id = p.author_id
    WHERE p.deleted_at IS NULL AND p.author_id = $1 AND ` + memoryAnyOf("p.posting_window_id", "wins") + `
      AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
    ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
  ),
  top AS MATERIALIZED (SELECT * FROM page ORDER BY posted_at DESC, id DESC LIMIT 50),
  rx AS MATERIALIZED (
    SELECT r.post_id, r.id AS "ID", r.image_id AS "ImageID", r.reaction_type AS "ReactionType",
      r.created_at AS "CreatedAt", ` + memoryInteractionUser + `
    FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
    WHERE ` + memoryAnyOf("r.post_id", "top") + ` AND r.replaced_at IS NULL AND r.deleted_at IS NULL
  ),
  cx AS MATERIALIZED (
    SELECT c.post_id, c.id AS "ID", c.body AS "Body", c.created_at AS "CreatedAt", ` + memoryInteractionUser + `
    FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
    WHERE ` + memoryAnyOf("c.post_id", "top") + ` AND c.deleted_at IS NULL
  )
  SELECT s.sess_region, p.id IS NOT NULL AS found, (SELECT count(*) > 50 FROM page) AS has_more,
    COALESCE(p.id, '') AS id, COALESCE(p.author_id, '') AS author_id,
    COALESCE(p.posting_window_id, '') AS posting_window_id, COALESCE(p.image_id, '') AS image_id,
    COALESCE(p.caption, '') AS caption, COALESCE(p.visibility, '') AS visibility,
    COALESCE(p.location_kind, '') AS location_kind, p.latitude, p.longitude, p.locality,
    COALESCE(p.posted_at, 'epoch') AS posted_at, COALESCE(p.on_time_deadline, 'epoch') AS on_time_deadline,
    p.window_ended_at, ` + userColumnsCoalesced("p", "author") + `,
    COALESCE((
      SELECT json_agg(r ORDER BY r."CreatedAt" DESC, r."ID" DESC) FROM (
        SELECT r."ID", r."ImageID", r."ReactionType", r."CreatedAt", r."User", r."SigningKey", r."ProfileImageID"
        FROM rx r WHERE r.post_id = p.id
        ORDER BY r."CreatedAt" DESC, r."ID" DESC LIMIT 50
      ) r
    ), '[]') AS reactions,
    COALESCE((
      SELECT json_agg(c ORDER BY c."CreatedAt", c."ID") FROM (
        SELECT c."ID", c."Body", c."CreatedAt", c."User", c."SigningKey", c."ProfileImageID"
        FROM cx c WHERE c.post_id = p.id
        ORDER BY c."CreatedAt", c."ID" LIMIT 50
      ) c
    ), '[]') AS comments
  FROM sess s LEFT JOIN top p ON true
  ORDER BY p.posted_at DESC, p.id DESC
`

func decodeMemoryInteractions(data []byte) ([]memoryInteraction, error) {
	var items []memoryInteraction
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	for i := range items {
		// pgx の通常の timestamptz 読み取りと同じローカル時刻表記にする。
		items[i].CreatedAt = items[i].CreatedAt.In(time.Local)
		items[i].User.SecretKeyHash = items[i].SigningKey
		items[i].User.ProfileImageID = items[i].ProfileImageID
		if err := items[i].User.setImageKey(); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func listMemories(c *echo.Context, h *HandlerContext, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, error) {
	var response getMemoriesResponse
	if (beforeAt == nil) != (beforeID == "") {
		return response, NewAPIError(http.StatusBadRequest, "invalid_pagination")
	}
	var posts []memoryPageRow
	if err := selectWithSession(c, h, &posts, func() (int, *string) {
		if len(posts) == 0 {
			return 0, nil
		}
		return len(posts), posts[0].SessRegion
	}, memoriesQuery, GetSession(c).UserID, start, end, beforeAt, beforeID); err != nil {
		return response, err
	}
	response.Items = make([]memoryPostResponse, 0, len(posts))
	plan := newSignaturePlan()
	reactions := make([][]memoryInteraction, len(posts))
	comments := make([][]memoryInteraction, len(posts))
	for i := range posts {
		post := &posts[i]
		response.HasMore = post.HasMore
		if !post.Found {
			continue
		}
		if err := post.Author.setImageKey(); err != nil {
			return response, err
		}
		var err error
		if reactions[i], err = decodeMemoryInteractions(post.ReactionsJSON); err != nil {
			return response, err
		}
		if comments[i], err = decodeMemoryInteractions(post.CommentsJSON); err != nil {
			return response, err
		}
		plan.add(post.Author, post.ImageID)
		plan.addProfile(post.Author)
		for _, reaction := range reactions[i] {
			plan.add(reaction.User, reaction.ImageID)
			plan.addProfile(reaction.User)
		}
		for _, comment := range comments[i] {
			plan.addProfile(comment.User)
		}
	}
	plan.run()
	for i, post := range posts {
		if !post.Found {
			continue
		}
		item := memoryPostResponse{
			createPostResponse: newPostResponse(post.Post, post.Author),
			RealMojis:          make([]replaceRealMojiResponse, 0, len(reactions[i])),
			Comments:           make([]createCommentResponse, 0, len(comments[i])),
		}
		for _, r := range reactions[i] {
			item.RealMojis = append(item.RealMojis, newRealMojiResponse(RealMoji{
				ID: r.ID, ImageID: r.ImageID, ReactionType: r.ReactionType, CreatedAt: r.CreatedAt,
			}, r.User))
		}
		for _, comment := range comments[i] {
			item.Comments = append(item.Comments, newCommentResponse(Comment{
				ID: comment.ID, Body: comment.Body, CreatedAt: comment.CreatedAt,
			}, comment.User))
		}
		response.Items = append(response.Items, item)
	}
	return response, nil
}
