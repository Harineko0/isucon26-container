#!/bin/bash
# nginx の LTSV ログ（infra/app/.../isureal.conf の log_format ltsv）を URI パターンごとに集計する。読むだけ。
# サーバ上で実行する。手元から: tr -d '\r' < alp.sh | ssh isucon26-asia 'bash -s'   （行数は bash -s 30 のように渡す）
#   列: count 2xx 3xx 4xx 5xx app_sum app_avg app_max request
#   app_* は apptime（upstream_response_time、秒）。並びは app_sum の降順。
#   SSE（GET /api/notifications）は接続中ずっと時間が積もるので、順位から外して最後に別枠で出す。
#   nginx が自分で返した（apptime が "-"）リクエストは apptime 0 として数える。
# env: LOG（既定 /var/log/nginx/access.log）, SSE（別枠にするキー。既定 "GET /api/notifications"）
N=${1:-40}
LOG=${LOG:-/var/log/nginx/access.log}
SSE=${SSE:-GET /api/notifications}
sudo cat "$LOG" | awk -F'\t' -v sse="$SSE" -v n="$N" '
{
  delete f
  for (i = 1; i <= NF; i++) { p = index($i, ":"); if (p) f[substr($i, 1, p - 1)] = substr($i, p + 1) }
  u = f["uri"]; sub(/\?.*/, "", u)
  # 数字を含む 16 文字以上のパス要素（UUID・ULID・hash など）、日付、数字だけの要素をまとめる
  m = split(u, seg, "/"); u = ""
  for (i = 2; i <= m; i++) {
    s = seg[i]
    if (s ~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/) s = ":date"
    else if (length(s) >= 16 && s ~ /[0-9]/) s = ":id"
    else if (s ~ /^[0-9]+$/) s = ":n"
    u = u "/" s
  }
  if (u == "") u = "/"
  key = f["method"] " " u
  t = f["apptime"] + 0 # "-" は 0、"0.1, 0.2"（upstream の再試行）は先頭の値
  c[key]++; sum[key] += t; if (t > mx[key]) mx[key] = t
  st[key, substr(f["status"], 1, 1)]++
}
function row(k) {
  return sprintf("%7d %6d %5d %5d %5d %9.2f %7.3f %7.3f  %s", c[k], st[k, 2], st[k, 3], st[k, 4], st[k, 5],
    sum[k], sum[k] / c[k], mx[k], k)
}
END {
  printf "%7s %6s %5s %5s %5s %9s %7s %7s  %s\n", "count", "2xx", "3xx", "4xx", "5xx", "app_sum", "app_avg", "app_max", "request"
  fflush()
  cmd = "sort -k6,6 -nr | head -n " n
  for (k in c) if (k != sse) print row(k) | cmd
  close(cmd)
  if (sse in c) { print "-- SSE（長時間接続。app_sum は負荷の指標にならない）"; print row(sse) }
}'
