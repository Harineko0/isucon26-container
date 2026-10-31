#!/bin/bash
# この run の PR コメントを作る/書き換える。PR が無い run（main の手動ベンチ）は何もしない。
#   comment.sh <markdown-file>
# env: PR_REPO, PR_NUMBER, COMMENT_TOKEN, STATE_DIR
set -eu
[ -n "${PR_NUMBER:-}" ] || exit 0
body=$(jq -Rs '{body: .}' <"$1")
id_file=$STATE_DIR/comment_id
if [ -s "$id_file" ]; then
  GH_TOKEN=$COMMENT_TOKEN gh api -X PATCH "repos/$PR_REPO/issues/comments/$(cat "$id_file")" \
    --input - <<<"$body" >/dev/null
else
  GH_TOKEN=$COMMENT_TOKEN gh api -X POST "repos/$PR_REPO/issues/$PR_NUMBER/comments" \
    --input - --jq .id <<<"$body" >"$id_file"
fi
