#!/bin/bash
# 複製スタック（PR の自作ベンチ用）を本番と同じテンプレートから作る。**人が実行する**（エージェントは使わない。hook が止める）。
#
#   ALLOW_STACK_CHANGE=1 ci/stacks/create.sh <name>      # 例: isucon-replica-1
#
# 作ったあと:
#   1. ci/stacks/<name>.env が書かれるので commit して PR（main に入ると CI が使える）
#   2. gh variable set BENCH_REPLICA_STACKS -R Harineko0/isucon26-container -b "isucon-replica-1 isucon-replica-2"（空白区切り）
#   3. 4 台の EBS 初期化が終わるのを待つ（manual.md「EBS の初期化の確認」。初期化中は I/O が遅くタイムアウトする）
#   4. 複製の 4 台も起動時にポータルへ自己登録するので、ポータルのサーバリストが混ざる。**16:30 の再起動リハーサルの前に
#      ci/stacks/delete.sh で消し、サーバリストが本番 4 台だけになっていることを確認する**（docs/final-runbook.md §0）。
# テンプレートはポータルから落としたチーム固有のもの（contest_cloudformation.yaml）。IAM リソースを作るので CAPABILITY_IAM を付ける。
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
name=${1:?usage: ALLOW_STACK_CHANGE=1 create.sh <name>}
if [ -n "${GITHUB_ACTIONS:-}" ] || [ "${ALLOW_STACK_CHANGE:-}" != 1 ]; then
  echo "ERROR: スタックの作成は人が手元で ALLOW_STACK_CHANGE=1 を付けて実行する（お金がかかり、ポータルのサーバリストに登録される）" >&2
  exit 1
fi
if [ "$name" = "${PROD_STACK:-isucon-prod-2}" ]; then
  echo "ERROR: $name は本番スタックの名前" >&2
  exit 1
fi
template=${TEMPLATE:-$root/contest_cloudformation.yaml}
P=(--region "${AWS_REGION:-ap-northeast-1}" --profile "${CFN_PROFILE:-AdministratorAccess-573111099202}")
aws cloudformation create-stack "${P[@]}" --stack-name "$name" --template-body "file://$template" \
  --capabilities CAPABILITY_IAM CAPABILITY_NAMED_IAM \
  --tags Key=isucon26-role,Value=replica Key=isucon26-created-by,Value="$(whoami)" >/dev/null
echo "create-stack $name: 待っています（数分）"
aws cloudformation wait stack-create-complete "${P[@]}" --stack-name "$name"
CFN_PROFILE=${CFN_PROFILE:-AdministratorAccess-573111099202} "$here/resolve.sh" --write "$name"
cat <<EOT

次にすること:
  git add ci/stacks/$name.env && git commit -m "ci: add replica stack $name" → PR → main
  gh variable set BENCH_REPLICA_STACKS -R Harineko0/isucon26-container -b "<いまの値> $name"
  EBS 初期化の完了を待つ。16:30 のリハーサル前に ALLOW_STACK_CHANGE=1 ci/stacks/delete.sh $name で消す。
EOT
