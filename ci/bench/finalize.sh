#!/bin/bash
# run の最後に必ず呼ぶ: 結果を bench-results ブランチの results.jsonl に追記し、最終コメントを書く。
# ベンチが FAIL / run が失敗なら exit 1。
set -u
B=$(cd "$(dirname "$0")" && pwd)
cd "$B/../.."

# ベンチを投げたあとに run が落ちた・キャンセルされた場合も、ジョブを記録に残す
if [ ! -s "$STATE_DIR/job.json" ] && [ -s "$STATE_DIR/job_id" ]; then
  python3 "$B/portal.py" show "$(cat "$STATE_DIR/job_id")" >"$STATE_DIR/job.json" || rm -f "$STATE_DIR/job.json"
fi

if [ -s "$STATE_DIR/job.json" ]; then
  line=$(python3 "$B/report.py" record)
  ok=
  for i in 1 2 3; do
    tmp=$(mktemp -d)
    if git fetch -q origin bench-results 2>/dev/null; then
      git worktree add -q "$tmp" FETCH_HEAD
    else
      git worktree add -q --detach "$tmp" && git -C "$tmp" checkout -q --orphan bench-results &&
        git -C "$tmp" rm -rqf . >/dev/null
    fi
    echo "$line" >>"$tmp/results.jsonl"
    git -C "$tmp" add results.jsonl
    git -C "$tmp" -c user.name=isucon-bench -c user.email=bench@users.noreply.github.com \
      commit -qm "bench #$(jq -r .id "$STATE_DIR/job.json"): $(jq -r .score "$STATE_DIR/job.json")"
    git -C "$tmp" push -q origin HEAD:refs/heads/bench-results && ok=1
    git worktree remove -f "$tmp"
    [ -n "${ok:-}" ] && break
  done
  [ -n "${ok:-}" ] || echo "warn: failed to push results"
fi

stage=$(cat "$STATE_DIR/stage" 2>/dev/null || echo failed:unknown)
case $stage in
  done | failed:* | rejected:*) ;;
  *) echo "failed:${stage%%:*}" >"$STATE_DIR/stage" ;; # キャンセル・タイムアウト
esac
python3 "$B/report.py" render >"$STATE_DIR/comment.md"
"$B/comment.sh" "$STATE_DIR/comment.md" || echo "warn: comment update failed"
cat "$STATE_DIR/comment.md" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"

[ "$(cat "$STATE_DIR/stage")" = done ] && jq -e '.result.passed' "$STATE_DIR/job.json" >/dev/null
