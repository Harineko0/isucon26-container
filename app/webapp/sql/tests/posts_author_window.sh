#!/bin/bash
# #77 の索引 posts(author_id, posting_window_id) の効果を、手元の Docker の PostgreSQL 16 で確かめる。
# 本番に近い形 (利用者 5000 人、3 リージョン × 365 日の投稿期間、1 人 1 期間 1 投稿で平均約 124 件、
# 最大 365 件、投稿は時刻順に並ぶ) のデータを作り、002-indexes.sql を当てる前後で
#   - Memories / 当日投稿済み判定のクエリの結果が同じこと
#   - posts 表から取り出すページ数 (pg_stat_get_xact_blocks_fetched) が、月の Memories で半分以下、
#     ほかで悪化しないこと
#     本番の Memories は、著者の投稿が 1 件ずつ別ページにあり、そのディスク読みで遅い (#77)。
#     バッファヒット数全体は、索引の根ページを毎回数えるので指標にしない。
#   - 002-indexes.sql を何度流しても失敗しないこと (initialize が毎回流す)
# を確かめる。使い方: app/webapp/sql/tests/posts_author_window.sh [比較対象の git ref (既定 origin/main)]
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
BASE_REF="${1:-origin/main}"
NAME="isureal-idx-test-$$"
PSQL=(docker exec -i "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)

wait_ready() {
	until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
	sleep 1
}
# プランナの設定は本番 (infra/db/.../conf.d/tuning.conf) に合わせる。
docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 \
	-c shared_buffers=768MB -c effective_cache_size=3GB -c work_mem=4MB -c jit=off \
	-c random_page_cost=1.1 -c effective_io_concurrency=200 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
wait_ready

"${PSQL[@]}" < 001-schema.sql >/dev/null
# 変更前の 002 (比較対象の ref のもの)
git show "$BASE_REF:app/webapp/sql/002-indexes.sql" | "${PSQL[@]}" >/dev/null 2>&1

"${PSQL[@]}" >/dev/null <<'SQL'
INSERT INTO users(id, username, secret_key_hash, display_name, region)
SELECT 'u' || i, 'user' || i, 'x', 'User ' || i, (ARRAY['asia','europe','america'])[1 + i % 3]
FROM generate_series(1, 5000) i;
INSERT INTO posting_windows(id, region, local_date, starts_at, on_time_deadline, ended_at)
SELECT 'w-' || r || '-' || d, r, date '2025-11-01' + d,
       (date '2025-11-01' + d)::timestamptz, (date '2025-11-01' + d)::timestamptz + interval '2 minutes',
       (date '2025-11-01' + d)::timestamptz + interval '10 minutes'
FROM generate_series(0, 364) d, unnest(ARRAY['asia','europe','america']) r;
-- 投稿は期間 (= 時刻) 順に入るので、1 人の投稿は別々のページに散る。u1..u30 は毎日投稿する。
-- random() の判定に d を混ぜて、(日, 人) ごとに評価させる (i 側だけに押し下げられないように)。
INSERT INTO posts(id, author_id, posting_window_id, image_id, caption, visibility, location_kind, posted_at)
SELECT 'p-' || d || '-' || i, 'u' || i, 'w-' || (ARRAY['asia','europe','america'])[1 + i % 3] || '-' || d,
       'img-' || d || '-' || i, repeat('c', 40), 'public', 'none',
       (date '2025-11-01' + d)::timestamptz + interval '1 minute'
FROM generate_series(0, 364) d, generate_series(1, 5000) i
WHERE i <= 30 OR random() + d * 0 < 0.34
ORDER BY d, random();
VACUUM ANALYZE;
SQL
echo "posts: $("${PSQL[@]}" -c "SELECT count(*) || ' rows, u1=' || count(*) FILTER (WHERE author_id='u1') || ', u4000=' || count(*) FILTER (WHERE author_id='u4000') FROM posts")"

# 削除済みの投稿も混ぜる (結果から除かれることの確認)。
"${PSQL[@]}" -c "UPDATE posts SET deleted_at = now() WHERE author_id IN ('u1', 'u4000') AND id LIKE 'p-3__-%' AND random() < 0.2" >/dev/null

# Memories: 変更前は post.go の selectPostsWithAuthor そのまま (posts JOIN posting_windows)、
# 変更後は memory_api.go の memoriesFrom (ファイルから取り出すので、コードとずれない)。
MEM_FROM_NEW=$(sed -n '/^const memoriesFrom = `/,/`$/p' ../go/memory_api.go | sed 's/^const memoriesFrom = `//; s/`$//')
[ -n "$MEM_FROM_NEW" ] || { echo "memoriesFrom not found in memory_api.go"; exit 1; }
mem() { # $1=FROM 句, $2=著者, $3=開始日, $4=終了日, $5=カーソル (posted_at, id) か NULL
	local cursor="TRUE" author="'$2'"
	[ "$5" = NULL ] || cursor="(p.posted_at, p.id) < ($5)"
	echo "SELECT p.id, p.posted_at, w.on_time_deadline FROM ${1//\$1/$author}
 JOIN users u ON u.id = p.author_id AND u.status = 'active'
 WHERE p.deleted_at IS NULL AND p.author_id = '$2' AND w.local_date >= '$3' AND w.local_date < '$4' AND $cursor
 ORDER BY p.posted_at DESC, p.id DESC LIMIT 51"
}
OLD_FROM="posts p JOIN posting_windows w ON w.id = p.posting_window_id"
CURSOR="'2026-10-20 00:01:00+00', 'p-353-1'"
# 004-functions.sql の当日投稿済み判定 (クエリは変えない。索引の差し替えで悪化しないことを見る)
POSTED="SELECT count(*) FILTER (WHERE p.deleted_at IS NULL), count(*) FILTER (WHERE p.deleted_at IS NOT NULL)
 FROM posts p JOIN posting_windows pw ON pw.id = p.posting_window_id
 WHERE p.author_id = 'u2' AND pw.local_date = '2026-10-15'"
OLD=(
"$(mem "$OLD_FROM" u1 2026-10-01 2026-11-01 NULL)"
"$(mem "$OLD_FROM" u4000 2026-10-01 2026-11-01 NULL)"
"$(mem "$OLD_FROM" u1 2026-10-01 2026-11-01 "$CURSOR")"
"$(mem "$OLD_FROM" u4999 2026-10-01 2026-11-01 NULL)"
"$(mem "$OLD_FROM" u1 2026-10-15 2026-10-16 NULL)"
"$POSTED"
)
NEW=(
"$(mem "$MEM_FROM_NEW" u1 2026-10-01 2026-11-01 NULL)"
"$(mem "$MEM_FROM_NEW" u4000 2026-10-01 2026-11-01 NULL)"
"$(mem "$MEM_FROM_NEW" u1 2026-10-01 2026-11-01 "$CURSOR")"
"$(mem "$MEM_FROM_NEW" u4999 2026-10-01 2026-11-01 NULL)"
"$(mem "$MEM_FROM_NEW" u1 2026-10-15 2026-10-16 NULL)"
"$POSTED"
)
NAMES=("memories month, daily author" "memories month, typical" "memories month, cursor" "memories month, u4999"
	"memories one day" "has posted today")
# 月の Memories は半分以下に。ほかは既存の索引で安いので、悪化しないこと。
EXPECT=(halve halve halve halve same same)
QUERIES=("${OLD[@]}")

measure() { # $1=query -> "<rows hash> <posts heap pages fetched> <warm ms>"
	local rows pages warm
	rows=$("${PSQL[@]}" -c "$1" | md5sum | cut -c1-12)
	# 同じトランザクションの中で、posts 表 (索引は含まない) から取り出したページ数を数える。
	pages=$(printf 'BEGIN;\n%s;\nSELECT pg_stat_get_xact_blocks_fetched(%s);\nCOMMIT;\n' "$1" "'posts'::regclass" |
		"${PSQL[@]}" | tail -1)
	# 温かい状態の実行時間は 5 回の最小値 (1 回だとぶれる)。合否には使わない。
	warm=$(for _ in 1 2 3 4 5; do "${PSQL[@]}" -c "EXPLAIN (ANALYZE, FORMAT JSON) $1" | tr -d '\n ' |
		grep -o '"ExecutionTime":[0-9.]*' | grep -o '[0-9.]*$'; done | sort -g | head -1)
	echo "$rows $pages $warm"
}

declare -a BEFORE
for i in "${!QUERIES[@]}"; do BEFORE[$i]=$(measure "${QUERIES[$i]}"); done

# 変更後の 002 を 2 回流す (initialize のたびに流れるので、何度でも成功する必要がある)。
"${PSQL[@]}" < 002-indexes.sql >/dev/null 2>&1
"${PSQL[@]}" < 002-indexes.sql >/dev/null 2>&1
"${PSQL[@]}" -c 'ANALYZE posts' >/dev/null
echo "posts indexes: $("${PSQL[@]}" -c "SELECT string_agg(indexname, ', ' ORDER BY indexname) FROM pg_indexes WHERE tablename = 'posts'")"

fail=0
printf '%-32s %12s %12s %10s %10s\n' query "pages before" "pages after" "warm ms b" "warm ms a"
for i in "${!QUERIES[@]}"; do
	after=$(measure "${NEW[$i]}")
	read -r rb cb wb <<<"${BEFORE[$i]}"
	read -r ra ca wa <<<"$after"
	printf '%-32s %12s %12s %10s %10s\n' "${NAMES[$i]}" "$cb" "$ca" "$wb" "$wa"
	[ "$rb" = "$ra" ] || { echo "  FAIL: result differs"; fail=1; }
	if [ "${EXPECT[$i]}" = halve ]; then
		[ "$ca" -le $((cb / 2)) ] || { echo "  FAIL: posts pages not halved"; fail=1; }
	else
		[ "$ca" -le "$cb" ] || { echo "  FAIL: posts pages increased"; fail=1; }
	fi
done
[ "$fail" = 0 ] && echo PASS || { echo FAIL; exit 1; }
