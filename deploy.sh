#!/bin/bash
# app/webapp の Go と SQL を 3 台のアプリサーバに配って再起動する。
# 3 台は並列に配り、1 台でも失敗したら（rsync・build・restart・起動確認のどれか）非 0 で終わる（#19）。
# 成功した台は最後に "<hostname> deployed" を出す（ci/bench/run.sh が 3 行あるか数える）。
set -eu
. "$(dirname "$0")/ci/guard.sh"
cd "$(dirname "$0")/app/webapp"
# 配った commit（このリポジトリの commit）を ~/webapp/DEPLOYED_SHA に残す（fingerprint.sh が読む）。
# サーバの /home/isucon は git 管理しない（コードはこのリポジトリだけにある）。昔の ~/.git が残っていれば消す。
SHA=$(git rev-parse HEAD)$(git diff --quiet HEAD -- . || echo -dirty)
KEY=~/.ssh/id_ed25519_github
HOSTS="${HOSTS:-13.159.52.104 52.199.206.161 52.197.40.86}"

# サーバ側: ビルド → 差し替え → 再起動のあと、新しいプロセスが :8080 を listen するまで待つ（最大 30 秒）。
# 起動直後に落ちる版を、ルーターが振り分けない europe1 / america1 でも検出するため。
# HTTP は叩かない（アクセスログ・action log・ip_bans の判定に余計なリクエストを混ぜない）。
REMOTE='set -e
cd ~/webapp/go
PATH=$HOME/local/go/bin:$PATH GOEXPERIMENT=simd go build -o isureal.new cmd/isureal/main.go
mv isureal.new isureal
rm -rf ~/.git
echo '"$SHA"' >~/webapp/DEPLOYED_SHA
sudo systemctl restart isureal-go
sudo truncate -s 0 /var/log/nginx/access.log
up=
for i in $(seq 30); do
  if ss -Hltn "sport = :8080" | grep -q . && systemctl is-active --quiet isureal-go; then up=1; break; fi
  sleep 1
done
if [ -z "$up" ]; then
  echo "$(hostname): isureal-go が 30 秒以内に :8080 を listen しない" >&2
  systemctl status --no-pager isureal-go 2>&1 | tail -n 15 >&2 || true
  exit 1
fi
echo "$(hostname) deployed"'

deploy_host() {
  local ip=$1
  rsync -a -e "ssh -i $KEY" --exclude /isureal --exclude /isureal.new go/ "isucon@$ip:/home/isucon/webapp/go/" ||
    { echo "[$ip] FAILED: rsync go/" >&2; return 1; }
  rsync -a -e "ssh -i $KEY" sql/ "isucon@$ip:/home/isucon/webapp/sql/" ||
    { echo "[$ip] FAILED: rsync sql/" >&2; return 1; }
  ssh -i "$KEY" "isucon@$ip" "$REMOTE" ||
    { echo "[$ip] FAILED: build / restart / 起動確認" >&2; return 1; }
}

pids=() ips=()
for ip in $HOSTS; do
  deploy_host "$ip" &
  pids+=($!) ips+=("$ip")
done
# 引数なしの wait は子の失敗を返さないので、PID ごとに待って失敗を集める
failed=
for i in "${!pids[@]}"; do
  wait "${pids[$i]}" || failed="$failed ${ips[$i]}"
done
if [ -n "$failed" ]; then
  echo "deploy.sh: FAILED on:$failed（app@$SHA は全台には入っていない。ベンチしないこと）" >&2
  exit 1
fi
echo "deploy.sh: OK on all hosts: app@$SHA"
