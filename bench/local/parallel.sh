#!/usr/bin/env bash
# 複数の commit (または作業ツリー) を、別々のスタックで同時にベンチする。
#
#   bench/local/parallel.sh origin/main my-branch        # 2 本を同時に測る
#   bench/local/parallel.sh --scale tiny HEAD HEAD~3     # 小さい初期データで
#   bench/local/parallel.sh -- -days 3 origin/main        # -- の後の 1 語目以降はベンチのフラグ…ではなく下記
#
# 書き方: parallel.sh [--scale S] [--bench "-days 3 -seed 1"] REF...
# REF に "." を書くと作業ツリー。スタックは p1, p2, … (サブネット 10.201…, 10.202…)。
# 1 台のマシンで走らせるので、絶対値ではなく同時に測った版同士の差を見る。
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
scale=small bench_flags=""
while [[ $# -gt 0 ]]; do
  case $1 in
    --scale) scale=$2; shift 2 ;;
    --bench) bench_flags=$2; shift 2 ;;
    *) break ;;
  esac
done
refs=("$@")
if (( ${#refs[@]} == 0 )); then
  echo "usage: parallel.sh [--scale S] [--bench FLAGS] REF..." >&2
  exit 2
fi

pids=() names=()
for i in "${!refs[@]}"; do
  ref=${refs[$i]}
  n=$((i + 1))
  name="p$n"
  names+=("$name")
  args=(-n "$name" -i "$n" --scale "$scale")
  [[ $ref != . ]] && args+=(-r "$ref")
  # shellcheck disable=SC2086
  "$here/run.sh" "${args[@]}" -- $bench_flags > "$here/work/$name.out" 2>&1 &
  pids+=($!)
  echo "started $name: $ref (log: bench/local/work/$name.out)"
done

status=0
for i in "${!pids[@]}"; do
  wait "${pids[$i]}" || status=1
done

printf '\n%-6s %-24s %-6s %-10s %s\n' stack ref pass score reason
for i in "${!refs[@]}"; do
  name=${names[$i]}
  python3 - "$here/work/$name/result.json" "$name" "${refs[$i]}" <<'PY'
import json, sys
path, name, ref = sys.argv[1:4]
try:
    r = json.load(open(path))
    print(f"{name:<6} {ref[:24]:<24} {'PASS' if r['pass'] else 'FAIL':<6} {r['score']:<10} {r.get('reason','')}")
except Exception as e:
    print(f"{name:<6} {ref[:24]:<24} {'-':<6} {'-':<10} no result ({e})")
PY
done
exit $status
