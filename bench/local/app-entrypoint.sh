#!/bin/bash
# リージョン間の遅延を tc netem で付けてから、isucon ユーザーでアプリを起動する。
# 本番は europe1/america1 ↔ asia (DB・asia1) の往復が約 100 ms。egress にだけ付けるので
# ISUREAL_NETEM_DELAY=100ms で往復 100 ms になる (NET_ADMIN が要る。無ければ飛ばす)。
set -eu
if [[ -n "${ISUREAL_NETEM_DELAY:-}" && "${ISUREAL_NETEM_DELAY}" != "0" && "${ISUREAL_NETEM_DELAY}" != "0ms" ]]; then
  if tc qdisc replace dev eth0 root netem delay "$ISUREAL_NETEM_DELAY" ${ISUREAL_NETEM_RATE:+rate "$ISUREAL_NETEM_RATE"} 2>/dev/null; then
    echo "netem: delay=$ISUREAL_NETEM_DELAY rate=${ISUREAL_NETEM_RATE:-none}" >&2
  else
    echo "netem: could not set qdisc (need NET_ADMIN); running without delay" >&2
  fi
fi
store="${ISUREAL_IMAGE_STORE_DIR:-/home/isucon/imgstore}" cache="${ISUREAL_IMAGE_CACHE_DIR:-/home/isucon/imgcache}"
mkdir -p "$store" "$cache" && chown isucon:isucon "$(dirname "$store")" "$store" "$cache" || true
# DB が起きるまで待つ
for _ in $(seq 1 60); do
  if PGPASSWORD="$ISUREAL_DB_PASSWORD" pg_isready -q -h "$ISUREAL_DB_HOST" -p "${ISUREAL_DB_PORT:-5432}" -U "$ISUREAL_DB_USER"; then break; fi
  sleep 1
done
exec setpriv --reuid=isucon --regid=isucon --init-groups --reset-env \
  env ISUREAL_REGION="$ISUREAL_REGION" ISUREAL_DB_HOST="$ISUREAL_DB_HOST" ISUREAL_DB_PORT="${ISUREAL_DB_PORT:-5432}" \
      ISUREAL_DB_USER="$ISUREAL_DB_USER" ISUREAL_DB_PASSWORD="$ISUREAL_DB_PASSWORD" ISUREAL_DB_NAME="$ISUREAL_DB_NAME" \
      ISUREAL_LOG_LEVEL="${ISUREAL_LOG_LEVEL:-info}" ISUREAL_LISTEN_HOST="${ISUREAL_LISTEN_HOST:-127.0.0.1}" \
      ISUREAL_PEER_HOSTS="${ISUREAL_PEER_HOSTS:-}" ISUREAL_APP_DB_POOL="${ISUREAL_APP_DB_POOL:-}" \
      ISUREAL_IMAGE_STORE_DIR="$store" ISUREAL_IMAGE_CACHE_DIR="$cache" \
      GOMEMLIMIT="${GOMEMLIMIT:-2GiB}" HOME=/home/isucon PATH=/usr/local/bin:/usr/bin:/bin \
  /home/isucon/webapp/go/isureal
