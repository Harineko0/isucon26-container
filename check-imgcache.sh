#!/bin/bash
# 3 台の画像ディスクキャッシュ (~/imgcache) の枚数を数える。初期データの画像は 1,826,950 枚。
# 足りないホストがあれば、ベンチが走っていないときにそのホストで imgsync を流す (#14)。
set -u
KEY=~/.ssh/id_ed25519_github
WANT=1826950
HOSTS="${HOSTS:-13.159.52.104 52.199.206.161 52.197.40.86}"
for ip in $HOSTS; do
  (
    n=$(ssh -i $KEY isucon@$ip 'find ~/imgcache -type f ! -name "*.tmp" 2>/dev/null | wc -l')
    if [ "$n" -ge "$WANT" ]; then echo "$ip ok $n"; else echo "$ip MISSING $n/$WANT"; fi
  ) &
done
wait
