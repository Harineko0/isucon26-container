#!/bin/bash
# DB に残っている物のうち、このリポジトリの sql/*.sql に名前が出てこない物を一覧にする (読み取りだけ)。
# PR のベンチが作ったトリガー・関数・表・索引・ALTER の設定は main の initialize では消えず、後の main のベンチに効く
# (2026-10-31: close 済み #225 の users のトリガーと cache_versions が main の DB 時間の 1 位になった。#249 で片付け)。
# 拡張 (pg_trgm など) の物とパーティション (008 が名前を作る) とその索引は除く。名前の照合は単語単位の grep なので、
# SQL のどこか (コメントや DROP ... IF EXISTS を含む) に名前が出てくれば「知っている物」として扱う。
#
# 使い方: app/webapp/sql/tests/leftover_objects.sh   (接続先は init.sh と同じく /home/isucon/env.sh か ISUREAL_DB_*)
# 終了コード: 0 = 残骸なし、1 = 残骸あり (一覧を表示)、2 = 接続できない
set -euo pipefail
cd -- "$(dirname -- "$0")/.."

if test -f /home/isucon/env.sh; then
	. /home/isucon/env.sh
fi
ISUREAL_DB_HOST=${ISUREAL_DB_HOST:-127.0.0.1}
ISUREAL_DB_PORT=${ISUREAL_DB_PORT:-5432}
ISUREAL_DB_USER=${ISUREAL_DB_USER:-isureal}
ISUREAL_DB_PASSWORD=${ISUREAL_DB_PASSWORD:-isureal}
ISUREAL_DB_NAME=${ISUREAL_DB_NAME:-isureal}

known=$(mktemp)
trap 'rm -f "$known"' EXIT
# sql/ の識別子らしい単語 (小文字に揃える)。
cat ./*.sql | tr 'A-Z' 'a-z' | grep -oE '[a-z_][a-z0-9_]*' | sort -u >"$known"

objects=$(PGPASSWORD="$ISUREAL_DB_PASSWORD" PGOPTIONS='-c default_transaction_read_only=on -c statement_timeout=5s' psql \
	--no-psqlrc --no-password -XAt -F '|' \
	--host="$ISUREAL_DB_HOST" --port="$ISUREAL_DB_PORT" --username="$ISUREAL_DB_USER" --dbname="$ISUREAL_DB_NAME" <<'SQL'
WITH ext AS (SELECT objid, classid FROM pg_depend WHERE deptype = 'e')
SELECT 'trigger', c.relname || '.' || t.tgname, lower(t.tgname)
FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE NOT t.tgisinternal AND n.nspname = 'public' AND NOT c.relispartition
UNION ALL
SELECT 'function', p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')', lower(p.proname)
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname = 'public' AND NOT EXISTS (SELECT 1 FROM ext WHERE ext.classid = 'pg_proc'::regclass AND ext.objid = p.oid)
UNION ALL
SELECT CASE c.relkind WHEN 'i' THEN 'index' WHEN 'I' THEN 'index' WHEN 'v' THEN 'view' WHEN 'm' THEN 'matview' WHEN 'S' THEN 'sequence' ELSE 'table' END,
  c.relname, lower(c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'i', 'I') AND NOT c.relispartition
  AND NOT EXISTS (SELECT 1 FROM ext WHERE ext.classid = 'pg_class'::regclass AND ext.objid = c.oid)
  -- 主キー・一意制約の索引と、シーケンスの所有者が表の列のもの (serial) は表と一緒に定義される。
  AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = c.oid)
  AND NOT (c.relkind = 'S' AND EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype IN ('a', 'i')))
UNION ALL
SELECT 'setting', coalesce(r.rolname, '*') || '@' || coalesce(d.datname, '*') || ' ' || s, 'setting'
FROM pg_db_role_setting rs
LEFT JOIN pg_roles r ON r.oid = rs.setrole
LEFT JOIN pg_database d ON d.oid = rs.setdatabase
CROSS JOIN LATERAL unnest(rs.setconfig) s
WHERE rs.setdatabase = 0 OR d.datname = current_database();
SQL
) || { echo "DB に接続できません" >&2; exit 2; }

leftovers=0
while IFS='|' read -r kind label name; do
	[ -z "$kind" ] && continue
	# ALTER ROLE / DATABASE の設定は sql/ では作らないので、あれば必ず表示する。
	if [ "$kind" = setting ] || ! grep -qxF "$name" "$known"; then
		echo "$kind $label"
		leftovers=$((leftovers + 1))
	fi
done <<<"$objects"

if [ "$leftovers" -gt 0 ]; then
	echo "sql/ に定義が無い物が $leftovers 件あります (PR のベンチの残骸の可能性)" >&2
	exit 1
fi
echo "残骸なし"
