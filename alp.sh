#!/bin/bash
# nginx の LTSV ログを URI パターンごとに集計する(count, sum, avg, max 秒)。
sudo cat /var/log/nginx/access.log | awk -F'\t' '
{
  for (i=1;i<=NF;i++){split($i,kv,":"); k=kv[1]; v=substr($i,length(k)+2); f[k]=v}
  u=f["uri"]; sub(/\?.*/,"",u)
  gsub(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/,":id",u)
  gsub(/[0-9]{4}-[0-9]{2}-[0-9]{2}/,":date",u)
  key=f["method"]" "u
  c[key]++; s[key]+=f["reqtime"]; if (f["reqtime"]>m[key]) m[key]=f["reqtime"]
  st[key" "f["status"]]++
}
END {
  for (k in c) printf "%6d %8.2f %6.3f %6.3f  %s\n", c[k], s[k], s[k]/c[k], m[k], k
}' | sort -k2 -nr | head -40
