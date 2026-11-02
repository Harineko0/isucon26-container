#!/bin/bash
# CloudFormation スタックの Outputs から 4 台とルーターの public IP を取り、ci/stacks/<name>.env の形で出す。
#
#   ci/stacks/resolve.sh <stack>            # stdout に env 形式で
#   ci/stacks/resolve.sh --write <stack>    # ci/stacks/<stack>.env に書く（複製スタックを作ったあとに人が実行して commit する）
#
# 資格情報: $CFN_PROFILE があれば --profile に使う。手元で AWS_PROFILE も環境変数の鍵も無ければ
# AdministratorAccess-573111099202（競技アカウント）を使う。CI（GITHUB_ACTIONS）では configure-aws-credentials の資格情報をそのまま使う。
# 読み取り専用（describe-stacks）。スタックは作らない・消さない。
set -eu
here=$(cd "$(dirname "$0")" && pwd)
write=
if [ "${1:-}" = --write ]; then write=1; shift; fi
name=${1:?usage: resolve.sh [--write] <stack>}
REGION=${AWS_REGION:-ap-northeast-1}
args=(--region "$REGION")
if [ -n "${CFN_PROFILE:-}" ]; then
  args+=(--profile "$CFN_PROFILE")
elif [ -z "${GITHUB_ACTIONS:-}" ] && [ -z "${AWS_PROFILE:-}" ] && [ -z "${AWS_ACCESS_KEY_ID:-}" ]; then
  args+=(--profile AdministratorAccess-573111099202)
fi

out=$(aws cloudformation describe-stacks --stack-name "$name" "${args[@]}" \
  --query 'Stacks[0].[StackStatus, join(" ", Outputs[].join("=", [OutputKey, OutputValue]))]' --output text) ||
  { echo "resolve.sh: describe-stacks $name に失敗（SSO が切れていれば aws sso login --profile AdministratorAccess-573111099202）" >&2; exit 1; }
status=$(head -n1 <<<"$out")
outputs=$(tail -n1 <<<"$out")
case $status in
  CREATE_COMPLETE | UPDATE_COMPLETE | UPDATE_ROLLBACK_COMPLETE) ;;
  *) echo "resolve.sh: $name は $status（CREATE_COMPLETE を待つ）" >&2; exit 1 ;;
esac
get() { tr ' ' '\n' <<<"$outputs" | sed -n "s/^$1=//p" | head -n1; }
kind=replica
[ "$name" = "${PROD_STACK:-isucon-prod-2}" ] && kind=prod
env=$(cat <<EOT
# CloudFormation $name（$REGION）。ci/stacks/resolve.sh --write $name が $(date -u +%FT%TZ) に生成。
STACK_NAME=$name
STACK_KIND=$kind
ASIA1_IP=$(get App1PublicIP)
EUROPE1_IP=$(get App2PublicIP)
AMERICA1_IP=$(get App3PublicIP)
ASIA2_IP=$(get DatabasePublicIP)
ROUTER_IP=$(get RouterPublicIP)
EOT
)
for k in ASIA1_IP EUROPE1_IP AMERICA1_IP ASIA2_IP ROUTER_IP; do
  grep -Eq "^$k=[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$" <<<"$env" || { echo "resolve.sh: $name の Outputs に $k が無い: $outputs" >&2; exit 1; }
done
if [ -n "$write" ]; then
  printf '%s\n' "$env" >"$here/$name.env"
  echo "wrote ci/stacks/$name.env"
  cat "$here/$name.env"
else
  printf '%s\n' "$env"
fi
