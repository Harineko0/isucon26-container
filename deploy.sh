#!/bin/bash
# app/webapp の Go と SQL を 3 台のアプリサーバに配って再起動する。
set -eu
. "$(dirname "$0")/ci/guard.sh"
cd "$(dirname "$0")/app/webapp"
KEY=~/.ssh/id_ed25519_github
HOSTS="${HOSTS:-13.159.52.104 52.199.206.161 52.197.40.86}"
for ip in $HOSTS; do
  (
    rsync -a -e "ssh -i $KEY" --exclude /isureal --exclude /isureal.new go/ isucon@$ip:/home/isucon/webapp/go/
    rsync -a -e "ssh -i $KEY" sql/ isucon@$ip:/home/isucon/webapp/sql/
    ssh -i $KEY isucon@$ip 'cd ~/webapp/go && PATH=$HOME/local/go/bin:$PATH GOEXPERIMENT=simd go build -o isureal.new cmd/isureal/main.go && mv isureal.new isureal && sudo systemctl restart isureal-go && sudo truncate -s 0 /var/log/nginx/access.log && echo "$(hostname) deployed"'
  ) &
done
wait
