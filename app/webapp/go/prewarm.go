package isureal

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
)

// DB (asia2: 2 vCPU、RAM 3.8GB、shared_buffers 768MB) の初期データは画像を除いても約 2GB あり、ベンチ前半は
// Memories の RealMoji 一覧・コメント一覧・投稿、画像の閲覧可否確認 (image_objects の主キーと heap) が
// 1 行ごとに別のページを EBS から読んでいた (pg_stat_statements: blk_read_time 119 秒 / total_exec_time 519 秒、
// 1 ブロックの読みに約 1.5〜2ms。asia2 の vmstat では iowait 30〜45%)。アプリの再起動 (デプロイ・追試の再起動) と
// initialize のたびに要求が来てから 1 ページずつ埋めるのをやめ、pg_prewarm でまとめて順に読んでおく
// (順読みは EBS のスループット上限で済み、1 ページずつのランダム読みより 1 桁速い)。
//
//   - 'buffer': shared_buffers に載せる。索引と小さい熱い表、posts の heap を優先度順に、shared_buffers の
//     prewarmBufferShare まで。
//   - 'read': 残り (realmojis / comments / image_objects の heap など) を OS のページキャッシュに載せる。
//     shared_buffers には入れない (入れても溢れるだけ)。PostgreSQL の読みはまず OS のキャッシュに当たる。
//
// notifications (heap 316MB + 索引 500MB) は通知 SSE が最新の投稿期間の行しか読まず、ベンチで disk 読みの上位に
// 出ないので載せない (載せると他を押し出す)。
//
// shared_preload_libraries は変えない (再起動が要り、CI のデプロイを壊した前歴がある #91)。CREATE EXTENSION と
// 関数呼び出しだけで済ませる (isureal は superuser)。initialize の応答は待たせず、別 goroutine で行う。
// 直前の prewarm から prewarmDebounce 以内なら飛ばす (CI の initialize → ベンチャーの initialize は数分以内に続き、
// その間にキャッシュは冷めない)。
const (
	prewarmDebounce    = 10 * time.Minute
	prewarmBufferShare = 0.8
	prewarmTimeout     = 3 * time.Minute
)

// 優先度順。前から shared_buffers の予算が尽きるまで 'buffer'、残りは 'read'。
// "image_objects_*" と "image_objects_*_pkey" は月パーティション (image_objects_YYYYMM) に展開する。
var prewarmCandidates = []string{
	// セッション・利用者・関係 (毎リクエスト)
	"users", "users_pkey", "users_username_key", "users_region_active",
	"login_sessions", "login_sessions_pkey", "login_sessions_active_token", "login_sessions_user_id_active",
	"administrator_sessions", "administrator_sessions_pkey", "administrator_sessions_token_hash",
	"posting_windows", "posting_windows_pkey", "posting_windows_one_active_region", "posting_windows_date_details",
	"posting_windows_region_local_date_key",
	"friendships", "friendships_pkey", "friendships_high_low", "friendships_user_low_id_user_high_id_key",
	"follows", "follows_pkey", "follows_follower_id_followee_id_key", "follows_followee_id",
	"block_relations", "block_relations_pkey", "block_relations_blocker_id_blocked_id_key", "block_relations_blocked_id",
	"friend_requests", "friend_requests_pkey", "friend_requests_pending_incoming", "friend_requests_pending_outgoing",
	"user_search_norms", "user_search_norms_pkey", "user_search_norms_username_trgm", "user_search_norms_display_name_trgm",
	"user_search_dirty", "user_search_dirty_pkey",
	// 投稿 (フィード・Memories・閲覧可否・当日投稿済み判定)
	"posts_pkey", "posts_author_window", "posts_image_id", "posts_posting_window_id", "posts",
	// Memories は投稿 ID から両方の一覧を引く。コメントと RealMoji の主キーは
	// 使われたページだけ通常の読み取りで載せ、全ページの先読みは省く。
	"comments_post_id", "realmojis_post_id",
	// 画像の可否確認とリアクションの照会。大きい索引・heap は予算を超えれば 'read' で読む。
	"realmojis_image_id", "realmojis_one_active", "image_objects_*_pkey",
	"comments", "realmojis", "image_objects_*",
}

var imageMonthlyPartition = regexp.MustCompile(`^image_objects_[0-9]{6}(_pkey)?$`)

var (
	prewarmRunning  atomic.Bool
	prewarmLastDone atomic.Int64 // 前回完了の UnixNano。0 は未実施
)

type prewarmRelation struct {
	OID    uint32 `db:"oid"`
	Name   string `db:"relname"`
	Blocks int64  `db:"blocks"`
}

// prewarmPlan は候補を実在する relation に解決し、'buffer' と 'read' に振り分ける。
// bufferBudget は shared_buffers のブロック数 × prewarmBufferShare。
func prewarmPlan(rels []prewarmRelation, bufferBudget int64) (buffer, read []prewarmRelation) {
	byName := make(map[string]prewarmRelation, len(rels))
	var imageHeaps, imageKeys []string
	for _, r := range rels {
		byName[r.Name] = r
		if imageMonthlyPartition.MatchString(r.Name) {
			if strings.HasSuffix(r.Name, "_pkey") {
				imageKeys = append(imageKeys, r.Name)
			} else {
				imageHeaps = append(imageHeaps, r.Name)
			}
		}
	}
	sort.Strings(imageHeaps)
	sort.Strings(imageKeys)
	var used int64
	seen := map[string]bool{}
	add := func(name string) {
		r, ok := byName[name]
		if !ok || r.Blocks == 0 || seen[name] {
			return
		}
		seen[name] = true
		if used+r.Blocks <= bufferBudget {
			used += r.Blocks
			buffer = append(buffer, r)
			return
		}
		read = append(read, r)
	}
	for _, name := range prewarmCandidates {
		switch name {
		case "image_objects_*":
			for _, n := range imageHeaps {
				add(n)
			}
		case "image_objects_*_pkey":
			for _, n := range imageKeys {
				add(n)
			}
		default:
			add(name)
		}
	}
	return buffer, read
}

// prewarmDB は上の計画どおりに pg_prewarm を呼ぶ。起動時 (asia) と initialize の後に別 goroutine で呼ぶ。
// 失敗しても stderr に出すだけで、リクエストには影響しない。
func prewarmDB(db *sqlx.DB, reason string) {
	if !prewarmRunning.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "prewarm(%s): skipped, already running\n", reason)
		return
	}
	defer prewarmRunning.Store(false)
	if last := prewarmLastDone.Load(); last != 0 && time.Since(time.Unix(0, last)) < prewarmDebounce {
		fmt.Fprintf(os.Stderr, "prewarm(%s): skipped, done %s ago\n", reason, time.Since(time.Unix(0, last)).Round(time.Second))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), prewarmTimeout)
	defer cancel()
	started := time.Now()

	if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS pg_prewarm`); err != nil {
		fmt.Fprintf(os.Stderr, "prewarm(%s): CREATE EXTENSION pg_prewarm failed: %v\n", reason, err)
		return
	}
	var sharedBuffers int64 // 8kB ブロック数
	if err := db.GetContext(ctx, &sharedBuffers, `SELECT setting::bigint FROM pg_settings WHERE name = 'shared_buffers'`); err != nil {
		fmt.Fprintf(os.Stderr, "prewarm(%s): shared_buffers: %v\n", reason, err)
		return
	}
	var rels []prewarmRelation
	if err := db.SelectContext(ctx, &rels, `
   SELECT c.oid, c.relname, pg_relation_size(c.oid) / 8192 AS blocks
   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = 'public' AND c.relkind IN ('r', 'i')
     AND (c.relname = ANY($1) OR c.relname ~ '^image_objects_[0-9]{6}(_pkey)?$')`,
		prewarmCandidates); err != nil {
		fmt.Fprintf(os.Stderr, "prewarm(%s): list relations: %v\n", reason, err)
		return
	}
	buffer, read := prewarmPlan(rels, int64(float64(sharedBuffers)*prewarmBufferShare))

	// 2 本の接続で 'buffer' と 'read' を並行に流す (EBS は並列に読むほうが速い。CPU は 1 コアずつ)。
	// 失敗した relation は飛ばして続ける (initialize 中に消えた索引など)。
	var wg sync.WaitGroup
	var counts [2]int64
	var done [2]int
	var errs [2]int
	for i, list := range [][]prewarmRelation{buffer, read} {
		mode := "buffer"
		if i == 1 {
			mode = "read"
		}
		wg.Add(1)
		go func(i int, mode string, list []prewarmRelation) {
			defer wg.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "prewarm(%s): conn: %v\n", reason, err)
				errs[i] = len(list)
				return
			}
			defer conn.Close()
			for _, r := range list {
				var n int64
				if err := conn.QueryRowContext(ctx, `SELECT pg_prewarm($1::oid::regclass, $2)`, r.OID, mode).Scan(&n); err != nil {
					fmt.Fprintf(os.Stderr, "prewarm(%s): %s %s: %v\n", reason, mode, r.Name, err)
					errs[i]++
					if ctx.Err() != nil {
						return
					}
					continue
				}
				counts[i] += n
				done[i]++
			}
		}(i, mode, list)
	}
	wg.Wait()
	if ctx.Err() == nil && errs[0] == 0 && errs[1] == 0 {
		prewarmLastDone.Store(time.Now().UnixNano())
	}
	fmt.Fprintf(os.Stderr, "prewarm(%s): buffer %d rels %dMB, read %d rels %dMB, errors %d, %s (shared_buffers %dMB)\n",
		reason, done[0], counts[0]*8/1024, done[1], counts[1]*8/1024, errs[0]+errs[1],
		time.Since(started).Round(100*time.Millisecond), sharedBuffers*8/1024)
}
