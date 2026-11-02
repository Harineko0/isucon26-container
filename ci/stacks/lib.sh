# スタック（CloudFormation の 1 組 = app 3 台 + asia2 + ルーター）の IP を読む。source して使う。
#
#   STACK=<name> . ci/stacks/lib.sh     # 既定は isucon-prod-2（本番）
#
# ci/stacks/<name>.env があればそれを読む。無ければ aws cloudformation describe-stacks の Outputs から
# 解決する（resolve.sh。AWS の資格情報が要る）。読んだあとは次の変数が使える:
#   STACK_NAME STACK_KIND(prod|replica) ASIA1_IP EUROPE1_IP AMERICA1_IP ASIA2_IP ROUTER_IP
#   ROUTER_URL（http://<ROUTER_IP>:8080） APP_IPS（app 3 台の public IP、空白区切り）
# プライベート IP（192.168.{1,2,3,4}.10）はテンプレートで固定なので、どのスタックでも同じ。
# infra/ の設定（nginx の map、pgbouncer.ini、env.sh の DB ホスト）はそのまま複製スタックでも使える。
STACKS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STACK=${STACK:-isucon-prod-2}
PROD_STACK=${PROD_STACK:-isucon-prod-2}

stack_load() {
  local f="$STACKS_DIR/$STACK.env" out
  if [ -f "$f" ]; then
    # shellcheck disable=SC1090
    . "$f"
  else
    out=$("$STACKS_DIR/resolve.sh" "$STACK") || {
      echo "stack $STACK: ci/stacks/$STACK.env が無く、CloudFormation からも解決できない" >&2
      return 1
    }
    eval "$out"
  fi
  : "${ASIA1_IP:?}" "${EUROPE1_IP:?}" "${AMERICA1_IP:?}" "${ASIA2_IP:?}" "${ROUTER_IP:?}"
  STACK_NAME=${STACK_NAME:-$STACK}
  if [ "$STACK_NAME" = "$PROD_STACK" ]; then STACK_KIND=prod; else STACK_KIND=${STACK_KIND:-replica}; fi
  ROUTER_URL=${ROUTER_URL:-http://$ROUTER_IP:8080}
  APP_IPS="$ASIA1_IP $EUROPE1_IP $AMERICA1_IP"
  export STACK STACK_NAME STACK_KIND ASIA1_IP EUROPE1_IP AMERICA1_IP ASIA2_IP ROUTER_IP ROUTER_URL APP_IPS
}

# 全スタックの IP（hook などが「サーバ」と見なす対象）を 1 行ずつ出す
stack_all_ips() {
  sed -nE 's/^(ASIA1_IP|EUROPE1_IP|AMERICA1_IP|ASIA2_IP|ROUTER_IP)=//p' "$STACKS_DIR"/*.env
}

stack_load
