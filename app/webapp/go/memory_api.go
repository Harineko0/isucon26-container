package isureal

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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
	ID           string
	Body         string
	ImageID      string
	ReactionType string
	CreatedAt    time.Time
	User         User
}

type memoryPageRow struct {
	SessRegion *string `db:"sess_region"`
	Found      bool    `db:"found"`
	HasMore    bool    `db:"has_more"`
	PostWithAuthor
	ReactionRecords []byte `db:"reactions"`
	CommentRecords  []byte `db:"comments"`
}

// The field order is shared with memoryInteraction.ScanIndex. Send the signing
// key as 32 bytes. The two length checks also reject whitespace, which
// PostgreSQL's hex decoder would otherwise ignore.
const memoryInteractionUser = `u.id, u.username, u.display_name, u.bio, u.account_type,
  CASE WHEN octet_length(u.secret_key_hash) = 64 THEN decode(u.secret_key_hash, 'hex') END,
  u.profile_image_id`

// pgx uses these destinations when it scans one anonymous ROW. Scanning into
// the result directly avoids building a second slice of reflective structs.
func (item *memoryInteraction) ScanIndex(i int) any {
	fields := [...]any{
		&item.ID, &item.Body, &item.ImageID, &item.ReactionType, &item.CreatedAt,
		&item.User.ID, &item.User.Username, &item.User.DisplayName, &item.User.Bio,
		&item.User.AccountType, &item.User.ImageKey, &item.User.ProfileImageID,
	}
	if i >= len(fields) {
		return nil
	}
	return fields[i]
}

func (*memoryInteraction) ScanNull() error { return errors.New("unexpected NULL memory interaction") }

// Reuse the pgx type registry without sharing it between concurrent decoders.
var memoryRecordMaps = sync.Pool{New: func() any { return pgtype.NewMap() }}

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
// リアクションとコメントは投稿別に順位を付け、各上位 50 件をまとめてからページに結合する。
// 投稿の 51 件は期間 ID で絞った時点で決まる。LIMIT の後で期間を主キーで引き、
// 日付範囲内の全期間と各投稿を改めて突き合わせる作業を省く。
// 読む索引は以前と同じ (posts_author_window, realmojis_post_id, comments_post_id)。リアクションの
// `COALESCE(replaced_at, deleted_at) IS NULL` は `replaced_at IS NULL AND deleted_at IS NULL` と同じ意味だが、
// 部分索引 realmojis_one_active (重複排除が効かず realmojis_post_id の約 3 倍の大きさで、キャッシュに
// 載りにくい。本番の idx_blks_read 84 万 vs 9 万) をプランナに選ばせないためにこう書く。
var memoriesQuery = `
  WITH sess AS MATERIALIZED (SELECT * FROM {session}),
  wins AS MATERIALIZED (
    SELECT w.id FROM posting_windows w WHERE w.local_date >= $2 AND w.local_date < $3
  ),
  page AS MATERIALIZED (
    SELECT ` + postColumns + `, ` + userColumnsAs("u", "author") + `
    FROM sess s JOIN users u ON u.id = s.sess_user_id AND u.status = 'active'
    CROSS JOIN LATERAL (
      SELECT * FROM posts p WHERE p.author_id = u.id AND ` + memoryAnyOf("p.posting_window_id", "wins") + `
        AND p.deleted_at IS NULL AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
      ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
    ) p
    JOIN posting_windows w ON w.id = p.posting_window_id
    WHERE u.id = $1
  ),
  top AS MATERIALIZED (SELECT * FROM page ORDER BY posted_at DESC, id DESC LIMIT 50),
  rx AS MATERIALIZED (
    SELECT post_id, array_send(array_agg(item ORDER BY created_at DESC, id DESC)) AS items
    FROM (
      SELECT r.post_id, r.created_at, r.id,
        ROW(r.id, ''::text, r.image_id, r.reaction_type, r.created_at, ` + memoryInteractionUser + `) AS item,
        row_number() OVER (PARTITION BY r.post_id ORDER BY r.created_at DESC, r.id DESC) AS n
      FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
      WHERE ` + memoryAnyOf("r.post_id", "top") + ` AND COALESCE(r.replaced_at, r.deleted_at) IS NULL
    ) ranked WHERE n <= 50 GROUP BY post_id
  ),
  cx AS MATERIALIZED (
    SELECT post_id, array_send(array_agg(item ORDER BY created_at, id)) AS items
    FROM (
      SELECT c.post_id, c.created_at, c.id,
        ROW(c.id, c.body, ''::varchar, ''::text, c.created_at, ` + memoryInteractionUser + `) AS item,
        row_number() OVER (PARTITION BY c.post_id ORDER BY c.created_at, c.id) AS n
      FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
      WHERE ` + memoryAnyOf("c.post_id", "top") + ` AND c.deleted_at IS NULL
    ) ranked WHERE n <= 50 GROUP BY post_id
  )
  SELECT s.sess_region, p.id IS NOT NULL AS found, (SELECT count(*) > 50 FROM page) AS has_more,
    COALESCE(p.id, '') AS id, COALESCE(p.author_id, '') AS author_id,
    COALESCE(p.posting_window_id, '') AS posting_window_id, COALESCE(p.image_id, '') AS image_id,
    COALESCE(p.caption, '') AS caption, COALESCE(p.visibility, '') AS visibility,
    COALESCE(p.location_kind, '') AS location_kind, p.latitude, p.longitude, p.locality,
    COALESCE(p.posted_at, 'epoch') AS posted_at, COALESCE(p.on_time_deadline, 'epoch') AS on_time_deadline,
    p.window_ended_at, ` + userColumnsCoalesced("p", "author") + `,
    rx.items AS reactions, cx.items AS comments
  FROM sess s LEFT JOIN top p ON true
  LEFT JOIN rx ON rx.post_id = p.id
  LEFT JOIN cx ON cx.post_id = p.id
  ORDER BY p.posted_at DESC, p.id DESC
`

func decodeMemoryInteractions(data []byte) ([]memoryInteraction, error) {
	if len(data) == 0 {
		return nil, nil
	}
	types := memoryRecordMaps.Get().(*pgtype.Map)
	defer memoryRecordMaps.Put(types)
	var items pgtype.FlatArray[memoryInteraction]
	if err := types.Scan(pgtype.RecordArrayOID, pgtype.BinaryFormatCode, data, &items); err != nil {
		return nil, err
	}
	for i := range items {
		items[i].CreatedAt = items[i].CreatedAt.In(time.Local)
		if len(items[i].User.ImageKey) != sha256.Size {
			return nil, errors.New("invalid image signing key")
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
		if reactions[i], err = decodeMemoryInteractions(post.ReactionRecords); err != nil {
			return response, err
		}
		if comments[i], err = decodeMemoryInteractions(post.CommentRecords); err != nil {
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
