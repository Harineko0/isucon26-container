package isureal

import (
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
		var rows []feedRow
		var query string
		var args []any
		if ranked {
			if deferred {
				query = feedDeferredQueries[input.Feed]
				args = []any{userID, input.BeforePostedAt, input.BeforeID, input.BeforeScore, h.Config.Region, tokenHash}
			} else {
				query = feedQueries[input.Feed]
				args = []any{userID, input.BeforePostedAt, input.BeforeID, input.BeforeScore}
			}
		} else {
			if deferred {
				query = feedDeferredQueries[input.Feed]
				args = []any{userID, input.BeforePostedAt, input.BeforeID, h.Config.Region, tokenHash}
			} else {
				query = feedQueries[input.Feed]
				args = []any{userID, input.BeforePostedAt, input.BeforeID}
			}
		}
		if err := sqlx.SelectContext(ctx, h.DB, &rows, query, args...); err != nil {
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

// feedOpenPostsCTE はどのフィードにも共通する条件（期間中・未削除・本人以外・閲覧者が投稿済み）の投稿。
// 開いている期間の投稿は数百件程度なので MATERIALIZED で先に取り出し、プランナが mutual などから
// posts_author_id を引く遅い計画（実測 1 秒超）を選ばないようにする。
// ここに著者の条件（友達・フォロー先）を入れると、著者から posts_author_id を引く計画になり遅くなる（実測 230ms）ので入れない。
var feedOpenPostsCTE = `
  open_posts AS MATERIALIZED (
    SELECT ` + postColumns + `
    FROM posts p
    JOIN posting_windows w ON w.id = p.posting_window_id AND w.ended_at IS NULL
    WHERE p.deleted_at IS NULL AND p.author_id <> $1
      AND COALESCE((SELECT has_posted_today FROM viewer), false)
  )`

// feedCandidateBase は open_posts に有効な著者を付け、ブロック関係（双方向）を除く。続けてフィードごとの条件を AND でつなぐ。
// 著者は 1 件ずつ主キーで引く（OFFSET 0 で平坦化を防ぎ、users の Seq Scan + Hash Join にさせない）。
// フィードごとの「著者 IN (...)」は `(...) IS TRUE` で包み、準結合への引き上げを防いで hashed SubPlan にする
// （引き上げられると汎用プランで open_posts × 友達の Nested Loop になり、実測 3.5ms かかった）。
// 著者 ID も一覧の ID も NULL にならないので、IS TRUE を付けても結果は変わらない。
var feedCandidateBase = `
    SELECT p.*, u.* FROM open_posts p
    CROSS JOIN LATERAL (
      SELECT ` + userColumnsAs("u", "author") + ` FROM users u
      WHERE u.id = p.author_id AND u.status = 'active' OFFSET 0
    ) u
    WHERE p.author_id NOT IN (SELECT id FROM blocked)`

// feedUnrankedCandidateBase は非ランキングフィード（public / friends / following）用の候補抽出。
// ここでは users をまだ引かず、feedUnrankedTail で posted_at, id 降順に並べてから先頭 51 件分の有効な著者だけを主キーで引く
// （open_posts 全件に対して先に users_pkey を引くと、51 件より後ろの候補まで毎回 users を引いていた）。
var feedUnrankedCandidateBase = `
    SELECT p.* FROM open_posts p
    WHERE p.author_id NOT IN (SELECT id FROM blocked)
      AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))`

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

// public: 公開投稿は誰でも見られるので、閲覧可否は visibility = 'public' だけで決まる。
func buildFeedPublicQuery(regionParam, tokenParam int) string {
	return `
  WITH` + feedViewerSessionCTE(regionParam, tokenParam) + `,` + feedBlockedCTE + `,` + feedOpenPostsCTE + `,
  candidates AS (` + feedUnrankedCandidateBase + `
      AND p.visibility = 'public'
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

// following: 著者がフォロー先で、閲覧可否は canView と同じ。
// 「著者が友達の友達（mutual）」は、著者と閲覧者の友達の間に友達関係があること（著者 <> 閲覧者は上で保証）。
func buildFeedFollowingQuery(regionParam, tokenParam int) string {
	return `
  WITH` + feedViewerSessionCTE(regionParam, tokenParam) + `,` + feedFriendsCTE + `,` + feedBlockedCTE + `,` + feedOpenPostsCTE + `,
  candidates AS (` + feedUnrankedCandidateBase + `
      AND (p.author_id IN (SELECT followee_id FROM follows WHERE follower_id = $1)) IS TRUE
      AND (p.visibility = 'public'
           OR (p.visibility = 'friends' AND p.author_id IN (SELECT id FROM friends))
           OR (p.visibility = 'friends_of_friends' AND (
                p.author_id IN (SELECT id FROM friends)
                OR EXISTS(SELECT 1 FROM friendships fs JOIN friends f ON f.id = fs.user_high_id WHERE fs.user_low_id = p.author_id)
                OR EXISTS(SELECT 1 FROM friendships fs JOIN friends f ON f.id = fs.user_low_id WHERE fs.user_high_id = p.author_id))))
  )` + feedUnrankedTail
}

// friends-of-friends: 著者は友達ではなく mutual に含まれる。この場合 friends 公開の投稿は見られない。
// おすすめ度の「著者の mutual 数」と「著者の友達の mutual 数の和」は、以前は候補ごとの相関副問い合わせで
// mutual（CTE、友達の多い閲覧者では千数百行）を毎回全件なめていた（候補 36 件で約 13ms）。
// mutual は id で一意なので LEFT JOIN、著者の友達の和は候補の著者ごとに 1 回だけ集計して LEFT JOIN する（値は同じ）。
//
// author_fof は「著者 → 和」を 1 つの jsonb にまとめ、候補ごとに ->> で引く（jsonb のキー検索は二分探索）。
// 以前の LEFT JOIN author_fof は、candidates の見積もりが数行（実際は友達の多い閲覧者で数百行）のため
// Nested Loop + Materialize になり、候補数 × 著者数の比較をしていた（候補 396 件で 7.8 万回）。
// 著者が author_fof に無い（著者の友達が mutual にいない）ときは ->> が NULL になり、以前の LEFT JOIN の NULL と同じく 0 になる。
// 反応・コメントの数は、友達かどうか（friends のハッシュ）を先に見て、友達だった利用者だけ active かを主キーで確かめる
// （以前は反応・コメントした全員の users を主キーで引いてから友達で絞っていた）。active は利用者ごとの性質なので、
// 「active な反応者・コメント者の集合 ∩ 友達」と「反応者・コメント者の集合 ∩ 友達 ∩ active」は同じ集合で、件数も同じ。
func buildFeedFriendsOfFriendsQuery(regionParam, tokenParam int) string {
	return `
  WITH` + feedViewerSessionCTE(regionParam, tokenParam) + `,` + feedFriendsCTE + `,` + feedBlockedCTE + `,
  mutual AS (
    SELECT id, count(*) AS n FROM (
      SELECT fs.user_high_id AS id FROM friendships fs JOIN friends f ON fs.user_low_id = f.id
      UNION ALL
      SELECT fs.user_low_id AS id FROM friendships fs JOIN friends f ON fs.user_high_id = f.id
    ) x WHERE id <> $1 GROUP BY id
  ),` + feedOpenPostsCTE + `,
  candidates AS (` + feedCandidateBase + `
      AND p.author_id NOT IN (SELECT id FROM friends) AND (p.author_id IN (SELECT id FROM mutual)) IS TRUE
      AND p.visibility IN ('public', 'friends_of_friends')
  ),
  author_fof AS (
    SELECT jsonb_object_agg(t.author_id, t.s) AS by_author FROM (
      SELECT a.author_id, sum(m.n) AS s
      FROM (SELECT DISTINCT author_id FROM candidates) a
      JOIN friendships fs ON fs.user_low_id = a.author_id OR fs.user_high_id = a.author_id
      JOIN mutual m ON m.id = CASE WHEN fs.user_low_id = a.author_id THEN fs.user_high_id ELSE fs.user_low_id END
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
            ) e WHERE e.user_id IN (SELECT id FROM friends)
                AND EXISTS(SELECT 1 FROM users eu WHERE eu.id = e.user_id AND eu.status = 'active'))
      + 3 * COALESCE(mm.n, 0)
      + LEAST(COALESCE(((SELECT by_author FROM author_fof) ->> c.author_id)::numeric, 0), 30)
      )::bigint AS score
    FROM candidates c
    LEFT JOIN mutual mm ON mm.id = c.author_id
  )
  SELECT v.sess_region, r.id IS NOT NULL AS found,
    ` + postWithAuthorColumnsCoalesced("r") + `,
    COALESCE(r.score, 0)::bigint AS score
  FROM viewer v
  LEFT JOIN LATERAL (
    SELECT * FROM scored
    WHERE $4::bigint IS NULL OR (score, posted_at, id) < ($4, $2::timestamptz, $3)
    ORDER BY score DESC, posted_at DESC, id DESC
    LIMIT 51
  ) r ON true
  ORDER BY r.score DESC, r.posted_at DESC, r.id DESC
`
}
