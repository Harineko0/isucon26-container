#!/bin/bash
# isureal_replace_realmoji / isureal_create_comment の users 行ロックを FOR NO KEY UPDATE → FOR SHARE にした変更の確認。
# 使い捨ての Docker の PostgreSQL 16 で行う (本番には接続しない)。
#   1. 新旧 (旧 = 下の BASE_SHA の定義を *_old という名前で入れる) が同じ状態から同じ結果・同じ書き込みになる (interaction_locks.sql)
#   2. 同じ投稿者へのコメント・RealMoji どうしは待たない (旧は待つ)
#   3. 退会・公開範囲の変更・region の変更・投稿・ブロックは、進行中のコメント・RealMoji を今までどおり待つ
#   4. 退会・ブロックの確定を待ったコメント・RealMoji は、確定後の状態で判定される (not_found / forbidden)
# 使い方: app/webapp/sql/tests/interaction_locks.sh        (このブランチの SQL)
#         BASE=1 app/webapp/sql/tests/interaction_locks.sh (修正前の SQL。2 が失敗するのが正しい)
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
BASE_SHA=a39b1e653301fc8ec52587170aa3ef70de9be6f4

NAME="isureal-interaction-locks-$$"
docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
sleep 1
PSQL=(docker exec -i -e PGOPTIONS='-c client_min_messages=warning' "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)

for file in 001-schema.sql 002-indexes.sql 007-image-location.sql 004-functions.sql 005-roundtrips.sql 006-image-storage.sql 007-feed-indexes.sql 008-image-partitions.sql; do
	"${PSQL[@]}" < "$file" >/dev/null
done
for fn in isureal_replace_realmoji isureal_create_comment; do
	git show "$BASE_SHA:app/webapp/sql/004-functions.sql" |
		sed -n "/^CREATE OR REPLACE FUNCTION $fn(/,/^END \$\$;/p" |
		sed "s/FUNCTION $fn(/FUNCTION ${fn}_old(/" | "${PSQL[@]}"
done
if [ "${BASE:-}" = 1 ]; then
	echo "mode: BASE ($BASE_SHA の SQL)"
	git show "$BASE_SHA:app/webapp/sql/004-functions.sql" | "${PSQL[@]}" >/dev/null
fi

echo "== 1. 新旧の結果の一致"
"${PSQL[@]}" < tests/interaction_locks.sql 2>&1

# 並行の確認用の利用者と投稿 (コミットする)。a: 投稿者 (公開の投稿 pa)、x1..x4: リアクションする人 (投稿済み)。
"${PSQL[@]}" <<'SQL'
DO $$
DECLARE w text := gen_random_uuid()::text; u text;
BEGIN
  INSERT INTO posting_windows(id, region, local_date, starts_at, on_time_deadline, ended_at)
  VALUES (w, 'asia', '2099-02-01', now(), now() + interval '2 min', NULL);
  FOREACH u IN ARRAY ARRAY['a', 'x1', 'x2', 'x3', 'x4'] LOOP
    INSERT INTO users(id, username, secret_key_hash, display_name, region) VALUES (u, u, 's', u, 'asia');
    INSERT INTO posts(id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at)
    VALUES ('p' || u, u, w, gen_random_uuid()::text, 'public', 'none', now());
  END LOOP;
END $$;
SQL

fail=0
# $1: 説明、$2: 先にロックを持つ側の SQL (3 秒保持)、$3: 後から来る側の SQL、$4: 期待 (wait / nowait)
check() {
	local desc=$1 holder=$2 waiter=$3 want=$4 out start end ms got
	"${PSQL[@]}" -c "BEGIN" -c "$holder" -c "SELECT pg_sleep(3)" -c "ROLLBACK" >/dev/null &
	local hpid=$!
	sleep 0.7
	start=$(python3 -c 'import time;print(time.time_ns())')
	out=$("${PSQL[@]}" -c "SET lock_timeout = '1s'" -c "BEGIN" -c "$waiter" -c "ROLLBACK" 2>&1 || true)
	end=$(python3 -c 'import time;print(time.time_ns())')
	wait $hpid
	ms=$(( (end - start) / 1000000 ))
	if grep -q "lock timeout" <<<"$out"; then got=wait; else got=nowait; fi
	if [ "$got" = "$want" ]; then echo "ok   $desc ($got, ${ms}ms)"; else echo "FAIL $desc (want $want, got $got, ${ms}ms): $out"; fail=1; fi
}
RM="SELECT o_result FROM isureal_replace_realmoji('x1', 'pa', gen_random_uuid()::text, gen_random_uuid()::text, '\x00', 'smile', NULL)"
CM="SELECT o_result FROM isureal_create_comment('x2', 'pa', gen_random_uuid()::text, 'hi')"

echo "== 2. 同じ投稿者へのコメント・RealMoji どうしは待たない"
check "RealMoji(x1→a) 中の RealMoji(x3→a)" "$RM" "SELECT o_result FROM isureal_replace_realmoji('x3', 'pa', gen_random_uuid()::text, gen_random_uuid()::text, '\x00', 'smile', NULL)" nowait
check "RealMoji(x1→a) 中のコメント(x2→a)" "$RM" "$CM" nowait
check "コメント(x2→a) 中のコメント(x4→a)" "$CM" "SELECT o_result FROM isureal_create_comment('x4', 'pa', gen_random_uuid()::text, 'hi')" nowait
check "コメント(x2→a) 中の投稿者自身のコメント(a→a)" "$CM" "SELECT o_result FROM isureal_create_comment('a', 'pa', gen_random_uuid()::text, 'hi')" nowait

echo "== 3. 退会・公開範囲・region・投稿・ブロックは進行中のコメント・RealMoji を待つ"
for holder in "$RM" "$CM"; do
	who=$([ "$holder" = "$RM" ] && echo "RealMoji(x1→a)" || echo "コメント(x2→a)")
	actor=$([ "$holder" = "$RM" ] && echo x1 || echo x2)
	check "$who 中の 投稿者の退会" "$holder" "SELECT isureal_delete_account('a')" wait
	check "$who 中の リアクションする人の退会" "$holder" "SELECT isureal_delete_account('$actor')" wait
	check "$who 中の 公開範囲の変更" "$holder" "SELECT isureal_update_post('a', 'pa', NULL, 'friends')" wait
	check "$who 中の 投稿の削除" "$holder" "SELECT isureal_delete_post('a', 'pa')" wait
	check "$who 中の 投稿者の region 変更" "$holder" "SELECT o_result FROM isureal_update_me('a', NULL, NULL, 'europe')" wait
	check "$who 中の リアクションする人の region 変更" "$holder" "SELECT o_result FROM isureal_update_me('$actor', NULL, NULL, 'europe')" wait
	check "$who 中の 投稿者のブロック" "$holder" "SELECT o_result FROM isureal_relationship('block', 'a', '$actor', gen_random_uuid()::text)" wait
	check "$who 中の 投稿者の新しい投稿" "$holder" "SELECT o_result FROM isureal_create_post('a', gen_random_uuid()::text, gen_random_uuid()::text, '\x00', '', 'public', 'none', NULL, NULL, NULL, NULL)" wait
done

echo "== 4. 待った後は確定後の状態で判定される"
# $1: 説明、$2: 先に確定させる側 (1.5 秒後にコミット)、$3: 後から来る側、$4: 期待する o_result
after() {
	local desc=$1 first=$2 second=$3 want=$4 got
	"${PSQL[@]}" -c "BEGIN" -c "$first" -c "SELECT pg_sleep(1.5)" -c "COMMIT" >/dev/null &
	local fpid=$!
	sleep 0.5
	got=$("${PSQL[@]}" -c "$second" 2>&1 | tail -1)
	wait $fpid
	if [ "$got" = "$want" ]; then echo "ok   $desc ($got)"; else echo "FAIL $desc (want $want, got $got)"; fail=1; fi
}
after "ブロック(a→x3) の確定を待った RealMoji(x3→a)" "SELECT o_result FROM isureal_relationship('block', 'a', 'x3', gen_random_uuid()::text)" \
	"SELECT o_result FROM isureal_replace_realmoji('x3', 'pa', gen_random_uuid()::text, gen_random_uuid()::text, '\x00', 'smile', NULL)" forbidden
after "ブロック(a→x3) 済みでのコメント(x3→a)" "SELECT 1" "SELECT o_result FROM isureal_create_comment('x3', 'pa', gen_random_uuid()::text, 'hi')" forbidden
after "公開範囲 friends への変更を待ったコメント(x4→a)" "SELECT isureal_update_post('a', 'pa', NULL, 'friends')" \
	"SELECT o_result FROM isureal_create_comment('x4', 'pa', gen_random_uuid()::text, 'hi')" forbidden
after "投稿者の退会を待った RealMoji(x1→a)" "SELECT isureal_delete_account('a')" \
	"SELECT o_result FROM isureal_replace_realmoji('x1', 'pa', gen_random_uuid()::text, gen_random_uuid()::text, '\x00', 'smile', NULL)" not_found
after "投稿者の退会後のコメント(x2→a)" "SELECT 1" "SELECT o_result FROM isureal_create_comment('x2', 'pa', gen_random_uuid()::text, 'hi')" forbidden
leaked=$("${PSQL[@]}" -c "SELECT count(*) FROM image_objects i WHERE NOT EXISTS (SELECT 1 FROM realmojis r WHERE r.image_id = i.id) AND owner_user_id LIKE 'x%'")
if [ "$leaked" = 0 ]; then echo "ok   失敗した RealMoji の画像は残らない"; else echo "FAIL 孤児の画像 $leaked 件"; fail=1; fi
exit $fail
