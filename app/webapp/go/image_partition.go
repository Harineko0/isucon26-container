package isureal

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// image_objects は created_at で 40 余りのパーティションに分かれている (001-schema.sql、008-image-partitions.sql)。
// 親表を引く汎用プランは、実行開始時に created_at で刈り込んでも、プランに含まれる全パーティションの
// 表ロック (AccessShare) を実行のたびに取る (PostgreSQL 16 の AcquireExecutorLocks)。1 回の画像取得で
// 45 前後の表ロックになり、バックエンドごとの fast-path (16 個) を超えた分が共有ロック表に行くので、
// ベンチ中の asia2 では LWLock:LockManager の待ちが最大の wait になっていた (1 秒に 54 バックエンド)。
// 画像の created_at は不変で、投稿画像なら posted_at と同じ値を応答時に覚えている (imageCreatedAt) ので、
// そこからパーティションの名前を求め、その表を直接引く (ロックは 1〜9 個)。外れていれば親表で引き直す。

// imagePartitionBoundary は 2026-10-28 06:00 UTC。これ以降 11 月までの画像は id のハッシュで 8 分割された
// 中間パーティション image_objects_20261028 に入る (008)。葉の名前は DB から教わる (tableoid) まで分からないので、
// 初回は中間パーティション (葉 8 + 自身 = 9 表) を引く。
var imagePartitionBoundary = time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)

var (
	imagePartitionsFrom = time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC)
	imagePartitionsTo   = time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC)
)

// imagePartitionFor は created_at の画像が入っているパーティションの名前を返す。
func imagePartitionFor(createdAt time.Time) string {
	t := createdAt.UTC()
	if t.Before(imagePartitionsFrom) || !t.Before(imagePartitionsTo) {
		return "image_objects_default"
	}
	if !t.Before(imagePartitionBoundary) && t.Before(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		return "image_objects_20261028"
	}
	return fmt.Sprintf("image_objects_%04d%02d", t.Year(), int(t.Month()))
}

// imagePartitionName は DB から教わった (tableoid::regclass::text) パーティション名の形。
var imagePartitionName = regexp.MustCompile(`^image_objects(_[0-9a-z_]+)?$`)

// imagePartitions は画像 id → それを持つ葉の表名 (DB から教わったもの)。1 件 100 バイト程度。
var imagePartitions = &boundedMap{limit: 2_000_000}

// imageRelationFor は画像 id を引くべき表の名前を返す。手がかりが無ければ "" (親表を引く)。
//
// 手がかりの表 (imagePartitions / imageCreatedAt) はサーバーごとなので、europe1/america1 経由で上げた画像を
// asia1 で初めて読むときや、追い出された後は手がかりが無く、親表 (全パーティション、ロック 92 個) を引いていた
// (前回のベンチで画像読み取り 13857 件中 1153 件 = 8%、平均 24 ms。葉・中間パーティション直接は 1.8〜2.9 ms)。
// 新しい画像の id は先頭にミリ秒時刻が入っている (ids.go の newTimeID) ので、created_at の代わりにそれから
// パーティションを求める。画像の created_at は DB の clock_timestamp() か投稿時刻で、id の時刻と数秒以内に
// 揃うので、月の境目以外は同じパーティションになる。外れていれば image_api.go が親表で引き直す。
func imageRelationFor(imageID string) string {
	if rel, ok := imagePartitions.Load(imageID); ok {
		return rel.(string)
	}
	if createdAt, ok := imageCreatedAt.Load(imageID); ok {
		return imagePartitionFor(createdAt.(time.Time))
	}
	if createdAt, ok := timeIDTime(imageID); ok {
		return imagePartitionFor(createdAt)
	}
	return ""
}

// rememberImagePartition は DB から教わった表名を覚える (葉でなければ無視する)。
func rememberImagePartition(imageID, rel string) {
	if rel == "" || rel == "image_objects" || !imagePartitionName.MatchString(rel) {
		return
	}
	if rel == "image_objects_20261028" {
		return // 中間パーティション。葉は _p0〜_p7
	}
	imagePartitions.Store(imageID, rel)
}

var imageQueryCache sync.Map // 表名 → SQL

// imageQueryFor は rel (image_objects かそのパーティション) から画像を引く SQL を返す。
// 署名鍵・閲覧可否・(未キャッシュなら) 本体・入っている表を 1 往復で取る。削除状態は毎回確認する。
func imageQueryFor(rel string) string {
	if q, ok := imageQueryCache.Load(rel); ok {
		return q.(string)
	}
	q := strings.ReplaceAll(`
   SELECT u.secret_key_hash, i.media_type, i.created_at, i.tableoid::regclass::text AS partition,
     CASE WHEN $2 THEN NULL ELSE i.data END AS data, i.stored_region, COALESCE(
     u.profile_image_id = i.id
     OR EXISTS (
       SELECT 1 FROM posts p
       WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL
     )
     OR EXISTS (
       SELECT 1 FROM realmojis r
       JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
       JOIN users author ON author.id = p.author_id AND author.status = 'active'
       WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
     )
   , false) AS allowed
   FROM {rel} i JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'
   WHERE i.id = $1`, "{rel}", rel)
	imageQueryCache.Store(rel, q)
	return q
}

// loadProfileImageHints はプロフィール画像の created_at と表名を覚える (起動時と initialize 後)。
// プロフィール画像は投稿や RealMoji のように応答時に created_at を知る機会が無く、初回の取得が親表の
// 全パーティションを引いていた。利用者 5 千人ぶんで 1 秒弱。
func loadProfileImageHints(db *sqlx.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var rows []struct {
		ID        string    `db:"id"`
		CreatedAt time.Time `db:"created_at"`
		Partition string    `db:"partition"`
	}
	if err := db.SelectContext(ctx, &rows, `
   SELECT i.id, i.created_at, i.tableoid::regclass::text AS partition
   FROM users u JOIN image_objects i ON i.id = u.profile_image_id
   WHERE u.profile_image_id IS NOT NULL
  `); err != nil {
		fmt.Fprintf(os.Stderr, "imgcache: failed to load profile image hints: %v\n", err)
		return
	}
	for _, row := range rows {
		imageCreatedAt.Store(row.ID, row.CreatedAt)
		rememberImagePartition(row.ID, row.Partition)
	}
}
