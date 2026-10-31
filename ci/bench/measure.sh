#!/bin/bash
# ベンチ 1 本分の計測（#37）。CI（run.sh）からだけ呼ぶ。失敗してもベンチは止めない（常に exit 0）。
#   measure.sh reset          pg_stat_statements と 3 台の access.log を空にする（ベンチ直前。状態を変えるので CI だけ）
#   measure.sh start DIR      4 台の vmstat と DB の wait event を 1 秒ごとにバックグラウンドで記録し始める
#   measure.sh stop DIR       記録を止める
#   measure.sh collect DIR    pg_stat_statements の上位・3 台の alp を取り、DIR/summary.md にまとめる
# 生データは DIR/*.txt（run のログにも出す）。summary.md は PR コメントに折りたたみで載る（report.py）。
set -u
B=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$B/../.." && pwd)
. "$ROOT/infra/lib.sh"
SSH="$SSH -n -o BatchMode=yes"
MAX_SEC=${MEASURE_MAX_SEC:-900} # 止め忘れても各サンプラーはこの秒数で終わる
APPS="asia1 europe1 america1"
DB=asia2
# sudo は環境変数を落とすので PGOPTIONS は sudo の内側で渡す
PSQL="sudo -u postgres env PGOPTIONS='-c statement_timeout=10s' psql -d isureal -AtX -v ON_ERROR_STOP=1"

cmd=${1:-}
dir=${2:-}

case $cmd in
  reset)
    . "$ROOT/ci/guard.sh"
    $SSH "$(host_addr $DB)" "$PSQL -c 'select pg_stat_statements_reset()'" >/dev/null &&
      echo "measure: pg_stat_statements reset" || echo "warn: measure: pg_stat_statements_reset failed"
    for h in $APPS; do
      $SSH "$(host_addr "$h")" 'sudo truncate -s 0 /var/log/nginx/access.log' ||
        echo "warn: measure: access.log truncate failed on $h"
    done
    ;;

  start)
    mkdir -p "$dir"
    : >"$dir/pids"
    for h in $APPS $DB; do
      timeout "$MAX_SEC" $SSH "$(host_addr "$h")" "vmstat -t 1 $MAX_SEC" >"$dir/vmstat-$h.txt" 2>>"$dir/stderr.log" &
      echo $! >>"$dir/pids"
    done
    # 1 秒ごとに、DB の backend が何をしているか（wait event なし = CPU、Lock = 行・表ロック待ち、IO = 読み書き待ち）。
    # 毎回 "." を出すので、接続が切れたら（stop）次の echo の SIGPIPE でサーバ側のループも終わる。
    q="select to_char(now(),'HH24:MI:SS'), coalesce(wait_event_type,'CPU'), coalesce(wait_event,'-'), count(*) from pg_stat_activity where datname='isureal' and state='active' and pid<>pg_backend_pid() group by 2,3 order by 4 desc"
    timeout "$MAX_SEC" $SSH "$(host_addr $DB)" \
      "for i in \$(seq $MAX_SEC); do $PSQL -c \"$q\"; echo .; sleep 1; done" >"$dir/db-waits.txt" 2>>"$dir/stderr.log" &
    echo $! >>"$dir/pids"
    date -u +%FT%TZ >"$dir/started_at"
    echo "measure: sampling started ($(wc -l <"$dir/pids") samplers)"
    ;;

  stop)
    [ -f "$dir/pids" ] && kill $(cat "$dir/pids") 2>/dev/null
    sleep 1
    echo "measure: sampling stopped"
    ;;

  collect)
    mkdir -p "$dir"
    timeout 60 $SSH "$(host_addr $DB)" "$PSQL -F' | ' -c \"select calls, round(total_exec_time) total_ms, round(mean_exec_time::numeric,2) mean_ms, rows, left(regexp_replace(query,'\\s+',' ','g'),150) from pg_stat_statements order by total_exec_time desc limit 30\"" \
      >"$dir/pg-top.txt" 2>>"$dir/stderr.log" || echo "warn: measure: pg_stat_statements failed"
    for h in $APPS; do
      tr -d '\r' <"$ROOT/alp.sh" | timeout 60 ssh -i "$SSH_KEY" -o ConnectTimeout=10 -o BatchMode=yes "$(host_addr "$h")" 'bash -s 20' \
        >"$dir/alp-$h.txt" 2>>"$dir/stderr.log" || echo "warn: measure: alp failed on $h"
    done

    {
      echo "app@${APP_SHA:-unknown} / job #$(cat "${STATE_DIR:-.}/job_id" 2>/dev/null || echo '?') / 計測開始 $(cat "$dir/started_at" 2>/dev/null || echo '?')"
      echo "（pg_stat_statements と access.log はベンチ直前に CI がリセット。この run の版の数字だけ）"
      echo
      echo "#### 版（ベンチ前の指紋: host DEPLOYED_SHA binary-sha256 起動時刻）"
      echo '```'
      cat "${STATE_DIR:-.}/fp-before.txt" 2>/dev/null
      echo '```'
      echo
      echo "#### CPU（負荷のある秒の平均 %）"
      echo '```'
      printf '%-9s %4s %4s %4s %4s\n' host us sy wa n
      for h in $APPS $DB; do
        # vmstat: us=13 sy=14 id=15 wa=16。us+sy が 5% 以下の秒（ベンチ前後のアイドル）は除く
        awk -v h="$h" '$1 ~ /^[0-9]+$/ && $13 + $14 > 5 { n++; us += $13; sy += $14; wa += $16 }
          END { if (n) printf "%-9s %4d %4d %4d %4d\n", h, us / n, sy / n, wa / n, n; else printf "%-9s (no data)\n", h }' \
          "$dir/vmstat-$h.txt" 2>/dev/null
      done
      echo '```'
      echo
      echo "#### DB wait event（active な backend 数の全サンプル合計。CPU = 待ちなし）"
      echo '```'
      awk -F'|' 'NF == 4 { s[$2 " " $3] += $4 } END { for (k in s) print s[k], k }' "$dir/db-waits.txt" 2>/dev/null |
        sort -rn | head -n 12
      echo '```'
      echo
      echo "#### pg_stat_statements（total_exec_time 上位 15）"
      echo '```'
      echo "calls | total_ms | mean_ms | rows | query"
      head -n 15 "$dir/pg-top.txt" | cut -c1-200
      echo '```'
      for h in $APPS; do
        echo
        echo "#### alp $h（apptime 合計順）"
        echo '```'
        cat "$dir/alp-$h.txt"
        echo '```'
      done
    } >"$dir/summary.md" 2>/dev/null
    for f in "$dir"/*.txt "$dir"/stderr.log; do [ -f "$f" ] && { echo "===== $(basename "$f")"; tail -n 200 "$f"; }; done
    ;;

  *)
    echo "usage: $0 reset | start DIR | stop DIR | collect DIR" >&2
    ;;
esac
exit 0
