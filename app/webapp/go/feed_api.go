package isureal

import (
	"context"
	"fmt"
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
	var friends friendFeedQueue
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
		// 閲覧可否・フィードの種類・おすすめ度・ページング・セッション確認をすべて DB 側で 1 文・接続 1 本で計算し、
		// 51 件だけを受け取る。フィードごとに別の SQL にして、不要な CTE（mutual など）の計画・実行を省く。
		tokenHash, deferred := deferredSession(c)
		userID := GetSession(c).UserID
		rows, err := selectFeedRows(ctx, h, &friends, input, userID, tokenHash, deferred)
		if err != nil {
			return err
		}
		var sessRegion *string
		if len(rows) > 0 {
			sessRegion = rows[0].SessRegion
		}
		if err := finishWithSession(c, h, tokenHash, deferred, len(rows) > 0, sessRegion); err != nil {
			return err
		}
		posts := make([]feedRow, 0, len(rows))
		for _, row := range rows {
			if !row.Found {
				continue
			}
			posts = append(posts, row)
		}
		hasMore := len(posts) > 50
		if hasMore {
			posts = posts[:50]
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
		return c.JSON(http.StatusOK, getFeedResponse{Items: items, HasMore: hasMore})
	}
}

func selectFeedRows(ctx context.Context, h *HandlerContext, friends *friendFeedQueue, input getFeedRequest, userID, tokenHash string, deferred bool) ([]feedRow, error) {
	if deferred && input.Feed == "friends" && input.BeforePostedAt == nil {
		return friends.selectRows(ctx, h.DB, h.Config.Region, friendFeedRequest{userID: userID, tokenHash: tokenHash})
	}

	query := feedQueries[input.Feed]
	args := []any{userID, input.BeforePostedAt, input.BeforeID}
	if input.Feed == "friends-of-friends" {
		args = append(args, input.BeforeScore)
	}
	if deferred {
		query = feedDeferredQueries[input.Feed]
		args = append(args, h.Config.Region, tokenHash)
	}
	var rows []feedRow
	err := sqlx.SelectContext(ctx, h.DB, &rows, query, args...)
	return rows, err
}

type feedRow struct {
	SessRegion *string `db:"sess_region"`
	Found      bool    `db:"found"`
	PostWithAuthor
	Score int64 `db:"score"`
}

// feedQueries は viewerContext.canView・matchesFeed・recommendationScores と同じ判定を SQL で行う。
// 以前は 4 種類を 1 本の SQL（CASE $2）で分岐していたが、汎用プランのコスト見積もりが大きく毎回カスタムプランで
// 再計画され（計画だけで 1.3〜5ms）、friends-of-friends では mutual から posts_author_id を引く遅い計画（1 秒超）も出た。
// 判定式は変えずにフィードごとに展開し、開いている期間の投稿を先に取り出す形に固定した。
//
// 非ランキング（public / following / friends）: $1 閲覧者, $2 beforePostedAt, $3 beforeId (deferred 時は $4 region, $5 tokenHash)
// friends-of-friends: $1 閲覧者, $2 beforePostedAt, $3 beforeId, $4 beforeScore (deferred 時は $5 region, $6 tokenHash)
var feedQueries = map[string]string{
	"public":             buildFeedPublicQuery(0, 0),
	"following":          buildFeedFollowingQuery(0, 0),
	"friends":            buildFeedFriendsQuery(0, 0),
	"friends-of-friends": buildFeedFriendsOfFriendsQuery(0, 0),
}

var feedDeferredQueries = map[string]string{
	"public":             buildFeedPublicQuery(4, 5),
	"following":          buildFeedFollowingQuery(4, 5),
	"friends":            buildFeedFriendsQuery(4, 5),
	"friends-of-friends": buildFeedFriendsOfFriendsQuery(5, 6),
}

// feedViewerSessionCTE は閲覧者が有効で、今日の期間に投稿済みかを返す。
// deferred 時は同じ 1 行の取得で login_sessions も確認し、リージョン違いなら has_posted_today を false にして
// open_posts の走査を省きつつ sess_region を返して 403 にする。
func feedViewerSessionCTE(regionParam, tokenParam int) string {
	if tokenParam == 0 {
		return `
  viewer AS MATERIALIZED (
    SELECT u.region AS sess_region,
      EXISTS(
        SELECT 1 FROM posts p
        JOIN posting_windows posted ON posted.id = p.posting_window_id
        JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                    AND current.local_date = posted.local_date
        WHERE p.author_id = u.id AND p.deleted_at IS NULL
      ) AS has_posted_today
    FROM users u WHERE u.id = $1 AND u.status = 'active'
  )`
	}
	return fmt.Sprintf(`
  viewer AS MATERIALIZED (
    SELECT u.region AS sess_region,
      (u.region = $%[1]d AND EXISTS(
        SELECT 1 FROM posts p
        JOIN posting_windows posted ON posted.id = p.posting_window_id
        JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                    AND current.local_date = posted.local_date
        WHERE p.author_id = u.id AND p.deleted_at IS NULL
      )) AS has_posted_today
    FROM users u
    JOIN login_sessions s ON s.user_id = u.id
    WHERE u.id = $1 AND u.status = 'active'
      AND s.token_hash = $%[2]d AND s.active = true
  )`, regionParam, tokenParam)
}

const feedFriendsCTE = `
  friends AS (
    SELECT CASE WHEN user_low_id = $1 THEN user_high_id ELSE user_low_id END AS id
    FROM friendships WHERE user_low_id = $1 OR user_high_id = $1
  )`

const feedBlockedCTE = `
  blocked AS (
    SELECT blocked_id AS id FROM block_relations WHERE blocker_id = $1
    UNION ALL
    SELECT blocker_id AS id FROM block_relations WHERE blocked_id = $1
  )`

// 非ランキングのフィードは score が常に 0 なので posted_at, id の降順だけで並ぶ。
// viewer から LEFT JOIN LATERAL して、一覧が 0 件でも有効なセッションなら番兵の 1 行を返す。
var feedUnrankedTail = `
  SELECT v.sess_region, r.id IS NOT NULL AS found,
    ` + postWithAuthorColumnsCoalesced("r") + `,
    0::bigint AS score
  FROM viewer v
  LEFT JOIN LATERAL (
    SELECT c.*, u.*
    FROM (
      SELECT * FROM candidates c
      ORDER BY c.posted_at DESC, c.id DESC
      OFFSET 0
    ) c
    CROSS JOIN LATERAL (
      SELECT ` + userColumnsAs("u", "author") + ` FROM users u
      WHERE u.id = c.author_id AND u.status = 'active' OFFSET 0
    ) u
    LIMIT 51
  ) r ON true
  ORDER BY r.posted_at DESC, r.id DESC
`

// 公開範囲とカーソルの条件を期間索引で投稿を読む段階で適用する。
// 非公開の投稿や前のページの本文を中間表に書き出す必要はない。
func buildFeedPublicQuery(regionParam, tokenParam int) string {
	return `
  WITH` + feedViewerSessionCTE(regionParam, tokenParam) + `,` + feedBlockedCTE + `,
  candidates AS (
    SELECT ` + postColumns + `
    FROM posting_windows w JOIN posts p ON p.posting_window_id = w.id
    WHERE w.ended_at IS NULL AND p.deleted_at IS NULL AND p.visibility = 'public'
      AND (SELECT has_posted_today FROM viewer) AND p.author_id <> $1
      AND p.author_id NOT IN (SELECT id FROM blocked)
      AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
  )` + feedUnrankedTail
}

// friends: 著者が友達なら public / friends / friends_of_friends のどれも閲覧できる。
//
// 以前は open_posts（開いている全期間・全リージョンの投稿）を MATERIALIZED で全件取り出してから友達で絞っていた。
// 利用者が 1,000 人を超えると開いている投稿も 1,000 件を超え、投稿直後のフィード確認（1 走行で 7,000 回超）ごとに
// その全件を組み立てて捨てていた（pg_stat_statements で平均 3.8ms、DB 時間の約 2 割）。
// 友達は数十人なので、友達 × 開いている期間ごとに posts_author_window (author_id, posting_window_id) を 1 回ずつ引く。
// 副問い合わせを LATERAL + OFFSET 0 にして平坦化を防ぎ、汎用プランでも必ず「友達 → 索引」の Nested Loop になるよう固定する
// （平坦化されると posts の Seq Scan + Hash Join を選びうる）。判定条件（期間中・未削除・本人以外・閲覧者が投稿済み・
// ブロック除外・ページング・公開範囲）は open_posts 版と同じで、結果の集合も並びも変わらない。
func buildFeedFriendsQuery(regionParam, tokenParam int) string {
	return `
  WITH` + feedViewerSessionCTE(regionParam, tokenParam) + `,` + feedFriendsCTE + `,` + feedBlockedCTE + `,
  candidates AS (
    SELECT p.* FROM friends f
    CROSS JOIN posting_windows w
    CROSS JOIN LATERAL (
      SELECT ` + postColumns + `
      FROM posts p
      WHERE p.author_id = f.id AND p.posting_window_id = w.id
        AND p.deleted_at IS NULL
        AND p.visibility IN ('public', 'friends', 'friends_of_friends')
      OFFSET 0
    ) p
    WHERE w.ended_at IS NULL
      AND COALESCE((SELECT has_posted_today FROM viewer), false)
      AND f.id <> $1
      AND f.id NOT IN (SELECT id FROM blocked)
      AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
  )` + feedUnrankedTail
}

// フォローが少なければ著者×期間を posts_author_window で直接引く。多いときは
// 全著者分の索引探索より開いている期間の投稿を一度読む方が安い。候補の絞り方だけを
// 切り替え、公開範囲やブロック・カーソルの判定は共通にする。PG16・有効投稿約 2,000 件の
// ローカル比較では 50 人で著者索引、500 人で期間索引が速かったので 64 人で切り替える。
func buildFeedFollowingQuery(regionParam, tokenParam int) string {
	return `
  WITH` + feedViewerSessionCTE(regionParam, tokenParam) + `,` + feedFriendsCTE + `,` + feedBlockedCTE + `,
  followees AS MATERIALIZED (
    SELECT followee_id FROM follows WHERE follower_id = $1
  ),
  many_followees AS (SELECT EXISTS(SELECT 1 FROM followees OFFSET 64) AS use_window_scan),
  following_posts AS (
    SELECT p.* FROM followees f
    CROSS JOIN posting_windows w
    CROSS JOIN LATERAL (
      SELECT ` + postColumns + `
      FROM posts p
      WHERE p.author_id = f.followee_id AND p.posting_window_id = w.id AND p.deleted_at IS NULL
      OFFSET 0
    ) p
    WHERE w.ended_at IS NULL AND NOT (SELECT use_window_scan FROM many_followees)
    UNION ALL
    SELECT ` + postColumns + `
    FROM posting_windows w JOIN posts p ON p.posting_window_id = w.id
    WHERE w.ended_at IS NULL AND p.deleted_at IS NULL AND (SELECT use_window_scan FROM many_followees)
      AND (p.author_id IN (SELECT followee_id FROM followees)) IS TRUE
  ),
  candidates AS (
    SELECT p.* FROM following_posts p
    WHERE (SELECT has_posted_today FROM viewer) AND p.author_id <> $1
      AND p.author_id NOT IN (SELECT id FROM blocked)
      AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
      AND (p.visibility = 'public'
           OR (p.visibility = 'friends' AND p.author_id IN (SELECT id FROM friends))
           OR (p.visibility = 'friends_of_friends' AND (
                p.author_id IN (SELECT id FROM friends)
                OR EXISTS(SELECT 1 FROM friendships fs JOIN friends f ON f.id = fs.user_high_id WHERE fs.user_low_id = p.author_id)
                OR EXISTS(SELECT 1 FROM friendships fs JOIN friends f ON f.id = fs.user_low_id WHERE fs.user_high_id = p.author_id))))
  )` + feedUnrankedTail
}

// ランキングは投稿 ID・著者 ID・投稿時刻だけで進める。スコアは著者の有効性とは独立なので、
// 順位順に有効な著者を 51 件まで取り、そのページの投稿本文と期間だけを主キーで引く。
//
// author_fof は著者ごとの 3 ホップの経路数を jsonb にまとめ、順位とカーソルが同じ集計を使えるよう実体化する。
// 友達関係の両方向はそれぞれ複合索引で引く。以前の OR 結合は自己辺を 1 行として数えるので、
// author_fof の 2 本目の枝だけで自己辺を除く。反応とコメントは有効な友達によるものだけを数える。
func buildFeedFriendsOfFriendsQuery(regionParam, tokenParam int) string {
	return `
  WITH` + feedViewerSessionCTE(regionParam, tokenParam) + `,` + feedFriendsCTE + `,` + feedBlockedCTE + `,
  active_friends AS MATERIALIZED (
    SELECT f.id FROM friends f JOIN users u ON u.id = f.id WHERE u.status = 'active'
  ),
  mutual AS (
    SELECT id, count(*) AS n FROM (
      SELECT fs.user_high_id AS id FROM friendships fs JOIN friends f ON fs.user_low_id = f.id
      UNION ALL
      SELECT fs.user_low_id AS id FROM friendships fs JOIN friends f ON fs.user_high_id = f.id
    ) x WHERE id <> $1 GROUP BY id
  ),
  open_posts AS MATERIALIZED (
    SELECT p.id, p.author_id, p.posted_at
    FROM posting_windows w JOIN posts p ON p.posting_window_id = w.id
    WHERE w.ended_at IS NULL AND p.deleted_at IS NULL AND p.author_id <> $1
      AND p.visibility IN ('public', 'friends_of_friends')
      AND (SELECT has_posted_today FROM viewer)
  ),
  candidates AS (
    SELECT p.* FROM open_posts p
    WHERE p.author_id NOT IN (SELECT id FROM blocked)
      AND p.author_id NOT IN (SELECT id FROM friends)
      AND (p.author_id IN (SELECT id FROM mutual)) IS TRUE
  ),
  author_fof AS MATERIALIZED (
    SELECT jsonb_object_agg(t.author_id, t.s) AS by_author FROM (
      SELECT a.author_id, sum(m.n) AS s
      FROM (SELECT DISTINCT author_id FROM candidates) a
      CROSS JOIN LATERAL (
        SELECT fs.user_high_id AS id FROM friendships fs WHERE fs.user_low_id = a.author_id
        UNION ALL
        SELECT fs.user_low_id FROM friendships fs WHERE fs.user_high_id = a.author_id AND fs.user_low_id <> a.author_id
      ) f
      JOIN mutual m ON m.id = f.id
      GROUP BY a.author_id
    ) t
  ),
  scored AS (
    SELECT c.*, (
      10 * (SELECT count(*) FROM (
              SELECT r.reactor_id AS user_id FROM realmojis r
              WHERE r.post_id = c.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
              UNION
              SELECT cm.author_id FROM comments cm
              WHERE cm.post_id = c.id AND cm.deleted_at IS NULL
            ) e WHERE (e.user_id IN (SELECT id FROM active_friends)) IS TRUE)
      + 3 * COALESCE(mm.n, 0)
      + LEAST(COALESCE(((SELECT by_author FROM author_fof) ->> c.author_id)::numeric, 0), 30)
      )::bigint AS score
    FROM candidates c
    LEFT JOIN mutual mm ON mm.id = c.author_id
  ),
  page AS (
    SELECT ranked.id AS page_id, ranked.score, u.*
    FROM (
      SELECT id, author_id, posted_at, score FROM scored
      WHERE $4::bigint IS NULL OR (score, posted_at, id) < ($4, $2::timestamptz, $3)
      ORDER BY score DESC, posted_at DESC, id DESC
      OFFSET 0
    ) ranked
    CROSS JOIN LATERAL (
      SELECT ` + userColumnsAs("u", "author") + ` FROM users u
      WHERE u.id = ranked.author_id AND u.status = 'active' OFFSET 0
    ) u
    ORDER BY ranked.score DESC, ranked.posted_at DESC, ranked.id DESC
    LIMIT 51
  )
  SELECT v.sess_region, r.id IS NOT NULL AS found,
    ` + postWithAuthorColumnsCoalesced("r") + `,
    COALESCE(r.score, 0)::bigint AS score
  FROM viewer v
  LEFT JOIN (
    SELECT ` + postColumns + `, k.*
    FROM page k JOIN posts p ON p.id = k.page_id
    JOIN posting_windows w ON w.id = p.posting_window_id
  ) r ON true
  ORDER BY r.score DESC, r.posted_at DESC, r.id DESC
`
}
