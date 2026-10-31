#!/bin/bash
# サーバの状態の指紋を出す。ベンチ前後で比べ、途中で誰かがデプロイ・再起動していないかを確かめる。
# 設定ファイルの中身は infra/deploy.sh -n（repo と一致するか）で別に確かめる。
set -eu
. "$(dirname "$0")/../../infra/lib.sh"

for host in $ALL_HOSTS; do
  if [ "$(host_role "$host")" = app ]; then
    cmd='sha256sum ~/webapp/go/isureal | cut -c1-16; systemctl show -p ActiveEnterTimestamp --value isureal-go nginx'
  else
    cmd='systemctl show -p ActiveEnterTimestamp --value postgresql@16-main'
  fi
  echo "$host $($SSH "$(host_addr "$host")" "$cmd" | tr '\n' ' ')"
done
