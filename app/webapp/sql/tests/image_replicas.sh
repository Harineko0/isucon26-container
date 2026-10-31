#!/usr/bin/env bash
# 1 つのローカルコンテナ内に PG16 と、別々のディスクを使う 3 地域のアプリを立てる。
# 依存: docker, go, curl, jq, cjpeg, unzip。競技サーバーやポータルには接続しない。
set -euo pipefail
cd "$(dirname "$0")/.."
name="isureal-image-replicas-$$"
tmp=$(mktemp -d)
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -r "$tmp"
}
trap cleanup EXIT

arch=$(docker version --format '{{.Server.Arch}}')
(cd ../go && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$tmp/isureal" ./cmd/isureal)
docker run -d --name "$name" -e POSTGRES_PASSWORD=test \
  -p 127.0.0.1::18081 -p 127.0.0.1::18082 -p 127.0.0.1::18083 \
  postgres:16 -c max_connections=210 >/dev/null
wait_for_db() {
  for attempt in {1..100}; do
    if docker exec "$name" pg_isready -h 127.0.0.1 -U postgres -q; then return 0; fi
    sleep 0.2
  done
  return 1
}
wait_for_db
docker exec "$name" mkdir -p /test/go
docker cp "$tmp/isureal" "$name:/test/go/isureal"
docker cp "$PWD" "$name:/test/sql"
sql=(docker exec -i "$name" psql -U postgres -v ON_ERROR_STOP=1 -qAtX)
"${sql[@]}" < 001-schema.sql >/dev/null
# reset の境界には初期データが少なくとも 1 行必要。
"${sql[@]}" <<'SQL'
INSERT INTO users (id, username, secret_key_hash, display_name, region)
VALUES ('original', 'original', repeat('ab', 32), 'Original', 'asia');
SQL
"${sql[@]}" < 003-initial-snapshot.sql >/dev/null
db_env=(-e ISUREAL_DB_USER=postgres -e ISUREAL_DB_PASSWORD=test -e ISUREAL_DB_NAME=postgres)
docker exec "${db_env[@]}" "$name" bash /test/sql/init.sh >/dev/null
peers=asia=127.0.0.1:18081,europe=127.0.0.1:18082,america=127.0.0.1:18083
start() {
  local region=$1 port=$2 address attempt
  docker exec -d -w /test/go "${db_env[@]}" \
    -e ISUREAL_REGION="$region" -e ISUREAL_PORT="$port" -e ISUREAL_PEER_HOSTS="$peers" \
    -e ISUREAL_IMAGE_STORE_DIR="/test/$region/store" -e ISUREAL_IMAGE_CACHE_DIR="/test/$region/cache" \
    -e ISUREAL_IMAGE_ACCEL=0 "$name" sh -c 'exec ./isureal >"/test/$ISUREAL_REGION.log" 2>&1'
  address=$(docker port "$name" "$port/tcp")
  for attempt in {1..100}; do
    if curl -s -o /dev/null --max-time 1 "http://$address/api/me"; then echo "http://$address"; return; fi
    sleep 0.1
  done
  docker exec "$name" cat "/test/$region.log" >&2
  return 1
}
asia=$(start asia 18081)
eu=$(start europe 18082)
us=$(start america 18083)
image_count() {
  docker exec "$name" find "/test/$1/store" -type f ! -name '.tmp-*' | wc -l
}
"${sql[@]}" <<'SQL'
INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
VALUES ('w', 'europe', current_date, now(), now() + interval '1 hour');
SQL

for color in red blu tan; do
  printf 'P6\n1 1\n255\n%s' "$color" | cjpeg > "$tmp/$color.jpg"
done
token() {
  local username=$1 key
  key=$(curl -fsS --json "{\"username\":\"$username\",\"displayName\":\"$username\",\"region\":\"europe\"}" \
    "$eu/api/auth/register" | jq -er .secretKey)
  curl -fsS --json "{\"username\":\"$username\",\"secretKey\":\"$key\"}" "$eu/api/auth/login" | jq -er .token
}
author="Authorization: Bearer $(token poster)"
reactor="Authorization: Bearer $(token reactor)"
curl -fsS -H "$author" -X PUT -H 'Content-Type: image/jpeg' \
  --data-binary "@$tmp/red.jpg" "$eu/api/me/profile-image"
profile=$(curl -fsS -H "$author" "$eu/api/me" | jq -er .profileImageUrl)
create_post() {
  curl -fsS -H "$1" -F "image=@$tmp/blu.jpg" -F caption=test -F visibility=public \
    -F location_kind=none "$eu/api/posts"
}
post=$(create_post "$author")
post_url=$(jq -er .imageUrl <<<"$post")
post_id=$(jq -er .id <<<"$post")
# 自分も投稿すると公開投稿にリアクションできる。
create_post "$reactor" >/dev/null
reaction=$(curl -fsS -H "$reactor" -F "image=@$tmp/tan.jpg" -F reactionType=like \
  "$eu/api/posts/$post_id/realmojis" | jq -er .imageUrl)

# 旧版の転送キューも動ける時間を置く。初回閲覧前に原本だけが永続化されていること。
sleep 1
[[ $(image_count europe) -eq 4 && $(image_count asia) -eq 0 && $(image_count america) -eq 0 ]]
# コンテナ再起動で PG と全 Go プロセスのメモリを落とす。3 地域とも同じ永続ファイルを再利用。
docker restart "$name" >/dev/null
wait_for_db
asia=$(start asia 18081)
eu=$(start europe 18082)
us=$(start america 18083)
curl -fsS "$asia$profile" | cmp "$tmp/red.jpg" -
curl -fsS "$asia$post_url" | cmp "$tmp/blu.jpg" -
curl -fsS "$asia$reaction" | cmp "$tmp/tan.jpg" -
for attempt in {1..30}; do
  [[ $(image_count asia) -eq 3 ]] && break
  sleep 0.1
done
[[ $(image_count asia) -eq 3 && $(image_count america) -eq 0 ]]
# 移動した利用者が新しい地域から、まだ当地に無い投稿画像を export できる。
curl -fsS -X PATCH -H "$author" --json '{"region":"america"}' "$eu/api/me" >/dev/null
curl -fsS -H "$author" "$us/api/me/export" > "$tmp/export.zip"
unzip -p "$tmp/export.zip" images/000001.jpg | cmp "$tmp/blu.jpg" -
curl -fsS "$us$post_url" | cmp "$tmp/blu.jpg" -
# 複製を持っていても不正署名・削除後は 403。
bad_url="${post_url%&sig=*}&sig=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
[[ $(curl -s -o /dev/null -w '%{http_code}' "$asia$bad_url") == 403 ]]
curl -fsS -X DELETE -H "$author" "$eu/api/me/profile-image"
[[ $(curl -s -o /dev/null -w '%{http_code}' "$asia$profile") == 403 ]]
curl -fsS -X POST "$us/api/initialize" | jq -e '.language == "go"' >/dev/null
for region in asia europe america; do [[ $(image_count "$region") -eq 0 ]]; done
[[ $(curl -s -o /dev/null -w '%{http_code}' "$asia$post_url") == 403 ]]
echo 'PASS: origin-only uploads, cold peer fetch, byte equality, cache invalidation and three-region reset'
