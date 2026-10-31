#!/bin/bash
# サーバ上の管理対象ファイルをリポジトリに取り込む。
#
#   infra/pull.sh -n [host...]           # 差分を表示するだけ（既定は全台）
#   infra/pull.sh asia1                  # asia1 の内容でリポジトリを上書き
#   infra/pull.sh asia2 /etc/foo.conf    # 新しいファイルを管理対象に加える
#
# 取り込み先は infra/hosts/<host>/ に同じファイルがあればそこ、無ければ infra/<role>/。
# app ロールは 3 台共通なので、europe1 などから取り込むと共通ファイルが上書きされる。
# 台ごとに変えたいファイルは infra/hosts/<host>/ に置く。
set -eu
. "$(dirname "$0")/lib.sh"

DRY=
if [ "${1:-}" = -n ]; then DRY=1; shift; fi

if [ $# -ge 2 ]; then
  HOSTS=$1; shift
  EXTRA=("$@")
else
  HOSTS="${*:-$ALL_HOSTS}"
  EXTRA=()
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for host in $HOSTS; do
  paths=$(tracked_paths "$host")
  for p in ${EXTRA[@]+"${EXTRA[@]}"}; do paths+=$'\n'"${p#/}"; done
  [ -n "$paths" ] || { echo "$host: no tracked files"; continue; }

  mkdir -p "$tmp/$host"
  # shellcheck disable=SC2086
  $SSH "$(host_addr "$host")" "sudo tar -C / -cf - $(echo $paths)" | tar -C "$tmp/$host" -xf -

  while read -r rel; do
    [ -n "$rel" ] || continue
    dst=$(repo_path "$host" "$rel")
    if [ -n "$DRY" ]; then
      diff -u --label "repo:$rel" --label "$host:/$rel" "$dst" "$tmp/$host/$rel" 2>/dev/null ||
        { [ -e "$dst" ] || echo "$host: /$rel is not in repo"; }
    elif ! cmp -s "$tmp/$host/$rel" "$dst" 2>/dev/null; then
      mkdir -p "$(dirname "$dst")"
      cp "$tmp/$host/$rel" "$dst"
      chmod u+rw,go+r "$dst"
      echo "$host: updated ${dst#"$INFRA_DIR"/}"
    fi
  done <<<"$paths"
done
