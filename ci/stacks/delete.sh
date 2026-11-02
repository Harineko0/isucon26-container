#!/bin/bash
# 複製スタックを消す。**人が実行する**（エージェントは使わない。hook が止める）。本番スタックは消せない。
#
#   ALLOW_STACK_CHANGE=1 ci/stacks/delete.sh <name>
#
# 消したあと: ci/stacks/<name>.env を消して commit、BENCH_REPLICA_STACKS から外す、
# ポータルのサーバリスト（https://portal.isucon.net/contestant/contestant_instances）が本番 4 台だけになっていることを確認する。
# 複製のサーバが登録した情報は、本番の 4 台が再起動して再登録するまで残ることがある（16:30 の再起動リハーサルで上書きされる）。
set -eu
here=$(cd "$(dirname "$0")" && pwd)
name=${1:?usage: ALLOW_STACK_CHANGE=1 delete.sh <name>}
if [ -n "${GITHUB_ACTIONS:-}" ] || [ "${ALLOW_STACK_CHANGE:-}" != 1 ]; then
  echo "ERROR: スタックの削除は人が手元で ALLOW_STACK_CHANGE=1 を付けて実行する" >&2
  exit 1
fi
if [ "$name" = "${PROD_STACK:-isucon-prod-2}" ]; then
  echo "ERROR: $name は本番スタック。消さない（追試ができず失格）" >&2
  exit 1
fi
P=(--region "${AWS_REGION:-ap-northeast-1}" --profile "${CFN_PROFILE:-AdministratorAccess-573111099202}")
# CI がそのスタックでベンチ中なら待つ（bench-locks ブランチの <name>.lock）
if gh api "repos/${GH_REPO:-Harineko0/isucon26-container}/contents/$name.lock?ref=bench-locks" >/dev/null 2>&1; then
  echo "WARN: CI が $name を使っている（bench-locks/$name.lock）。run が終わるのを待つか、Actions でキャンセルしてから消す" >&2
  [ "${FORCE:-}" = 1 ] || exit 1
fi
aws cloudformation delete-stack "${P[@]}" --stack-name "$name"
echo "delete-stack $name: 待っています"
aws cloudformation wait stack-delete-complete "${P[@]}" --stack-name "$name"
rm -f "$here/$name.env"
cat <<EOT
消しました。次にすること:
  git rm ci/stacks/$name.env（消えていれば）→ commit
  gh variable set BENCH_REPLICA_STACKS -R Harineko0/isucon26-container -b "<残りの名前>"（最後の 1 つなら空文字）
  ci/stacks/check.sh で残りのスタックが本番だけか確かめ、ポータルのサーバリストを目で見る
EOT
