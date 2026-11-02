#!/bin/bash
# 複製スタックの消し忘れを検知する（読み取りだけ）。本番以外のスタックがあれば一覧を出して exit 1。
#
#   ci/stacks/check.sh            # 手元（AdministratorAccess-573111099202）
#   CI: .github/workflows/replica-check.yml が定期的に実行し、残っていれば Slack / Issue に警告する
#
# 出すもの: CloudFormation のスタック一覧（削除済み以外）と、各スタックの EC2 の public IP。
# ポータルのサーバリスト（https://portal.isucon.net/contestant/contestant_instances）に出ている IP が
# 本番（ci/stacks/isucon-prod-2.env）の 4 台と一致することは、人がブラウザで確かめる。
set -eu
here=$(cd "$(dirname "$0")" && pwd)
prod=${PROD_STACK:-isucon-prod-2}
args=(--region "${AWS_REGION:-ap-northeast-1}")
if [ -n "${CFN_PROFILE:-}" ]; then
  args+=(--profile "$CFN_PROFILE")
elif [ -z "${GITHUB_ACTIONS:-}" ] && [ -z "${AWS_PROFILE:-}" ] && [ -z "${AWS_ACCESS_KEY_ID:-}" ]; then
  args+=(--profile AdministratorAccess-573111099202)
fi
stacks=$(aws cloudformation list-stacks "${args[@]}" \
  --stack-status-filter CREATE_IN_PROGRESS CREATE_COMPLETE CREATE_FAILED ROLLBACK_IN_PROGRESS ROLLBACK_FAILED ROLLBACK_COMPLETE \
    UPDATE_IN_PROGRESS UPDATE_COMPLETE UPDATE_ROLLBACK_COMPLETE DELETE_IN_PROGRESS DELETE_FAILED \
  --query 'StackSummaries[].[StackName,StackStatus,CreationTime]' --output text)
echo "CloudFormation stacks (${AWS_REGION:-ap-northeast-1}):"
printf '%s\n' "$stacks" | sed 's/^/  /'
extra=$(awk -v p="$prod" '$1 != p && $2 != "DELETE_IN_PROGRESS" {print $1}' <<<"$stacks")
echo
echo "EC2 public IPs by stack:"
aws ec2 describe-instances "${args[@]}" --filters Name=instance-state-name,Values=pending,running,stopping,stopped \
  --query 'Reservations[].Instances[].[Tags[?Key==`aws:cloudformation:stack-name`]|[0].Value, Tags[?Key==`aws:cloudformation:logical-id`]|[0].Value, State.Name, PublicIpAddress]' \
  --output text | sort | sed 's/^/  /'
echo
if [ -n "$extra" ]; then
  echo "WARNING: 本番 ($prod) 以外のスタックが残っている: $(paste -sd' ' - <<<"$extra")"
  echo "  16:30 の再起動リハーサル前に ALLOW_STACK_CHANGE=1 ci/stacks/delete.sh <name> で消す。"
  echo "  消したあと、ポータルのサーバリストが本番 4 台だけか確かめる（docs/final-runbook.md §0）。"
  exit 1
fi
echo "OK: スタックは $prod だけ"
