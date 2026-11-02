#!/bin/bash
# 複製スタックを 1 つ占有する / 返す（PR の自作ベンチを並列に回すため。1 スタックに同時に 1 run）。
#
#   stacklock.sh claim     # BENCH_REPLICA_STACKS（空白区切り）から空いているものを取り、名前を stdout に出す。空くまで待つ
#   stacklock.sh release   # この run が取ったロックを返す（finalize.sh が必ず呼ぶ）
#
# ロックは bench-locks ブランチの <stack>.lock（JSON: run_id, run_url, label, claimed_at）。
# GitHub の contents API は「無いファイルの作成」が原子的（既にあれば 422）なので、これで排他する。
# 持ち主の run が completed なら（runner が死んだ等）古いロックとみなして消す。
# env: GH_TOKEN, GITHUB_REPOSITORY, GITHUB_RUN_ID, GITHUB_RUN_NUMBER, STATE_DIR, BENCH_REPLICA_STACKS, LABEL
#      ON_WAIT: 待つたびに "$ON_WAIT <msg>" を実行する（PR コメントの更新用）
set -euo pipefail
BRANCH=${LOCK_BRANCH:-bench-locks}
API="repos/$GITHUB_REPOSITORY/contents"
me=${GITHUB_RUN_ID:?}

ensure_branch() {
  gh api "repos/$GITHUB_REPOSITORY/branches/$BRANCH" >/dev/null 2>&1 && return 0
  local tmp
  tmp=$(mktemp -d)
  git init -q "$tmp"
  git -C "$tmp" -c user.name=isucon-bench -c user.email=bench@users.noreply.github.com commit -q --allow-empty -m "bench-locks: 複製スタックのロック置き場"
  git -C "$tmp" push -q "https://x-access-token:$GH_TOKEN@github.com/$GITHUB_REPOSITORY" HEAD:refs/heads/$BRANCH || true
  rm -rf "$tmp"
}

# 既存ロックを読む。stdout: "<sha> <run_id>"（無ければ空）
read_lock() {
  gh api "$API/$1.lock?ref=$BRANCH" --jq '[.sha, (.content | @base64d | fromjson | .run_id)] | @tsv' 2>/dev/null | tr '\t' ' ' || true
}

try_claim() {
  local stack=$1 existing sha owner status body
  existing=$(read_lock "$stack")
  if [ -n "$existing" ]; then
    sha=${existing%% *} owner=${existing##* }
    [ "$owner" = "$me" ] && { echo "$stack"; return 0; }
    status=$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$owner" --jq .status 2>/dev/null || echo completed)
    [ "$status" = completed ] || return 1
    echo "stale lock on $stack (run $owner is $status): removing" >&2
    gh api -X DELETE "$API/$stack.lock" -f message="bench-locks: stale $stack (run $owner)" -f sha="$sha" -f branch="$BRANCH" >/dev/null 2>&1 || return 1
  fi
  body=$(jq -n --arg run_id "$me" --arg run_number "${GITHUB_RUN_NUMBER:-}" --arg label "${LABEL:-}" \
    --arg run_url "${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY/actions/runs/$me" \
    --arg claimed_at "$(date -u +%FT%TZ)" '{run_id: ($run_id | tonumber), run_number: $run_number, label: $label, run_url: $run_url, claimed_at: $claimed_at}')
  if gh api -X PUT "$API/$stack.lock" -f message="bench-locks: $stack ← ${LABEL:-} (run $me)" \
    -f content="$(base64 <<<"$body" | tr -d '\n')" -f branch="$BRANCH" >/dev/null 2>&1; then
    echo "$stack"
    return 0
  fi
  return 1
}

case ${1:-} in
  claim)
    [ -n "${BENCH_REPLICA_STACKS:-}" ] || { echo "stacklock: BENCH_REPLICA_STACKS が空" >&2; exit 1; }
    ensure_branch
    prev=
    while :; do
      for stack in $BENCH_REPLICA_STACKS; do
        if got=$(try_claim "$stack"); then
          echo "$got" >"$STATE_DIR/stack"
          echo "$got"
          exit 0
        fi
      done
      msg="all replica stacks busy: $BENCH_REPLICA_STACKS"
      if [ "$msg" != "$prev" ]; then
        echo "$msg" >&2
        [ -z "${ON_WAIT:-}" ] || $ON_WAIT "$(wc -w <<<"$BENCH_REPLICA_STACKS" | tr -d ' ')" || true
        prev=$msg
      fi
      sleep 15
    done
    ;;
  release)
    stack=$(cat "$STATE_DIR/stack" 2>/dev/null || true)
    [ -n "$stack" ] || exit 0
    existing=$(read_lock "$stack")
    [ -n "$existing" ] || exit 0
    sha=${existing%% *} owner=${existing##* }
    [ "$owner" = "$me" ] || { echo "stacklock: $stack is held by run $owner, not releasing" >&2; exit 0; }
    gh api -X DELETE "$API/$stack.lock" -f message="bench-locks: release $stack (run $me)" -f sha="$sha" -f branch="$BRANCH" >/dev/null &&
      echo "released $stack" || echo "warn: release $stack failed" >&2
    ;;
  *)
    echo "usage: $0 claim | release" >&2
    exit 2
    ;;
esac
