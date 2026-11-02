#!/bin/bash
# stacklock.sh の単体テスト。gh を偽物（ローカルのディレクトリを contents API に見せる）に差し替えて、
# 空いているスタックを取る / 2 本目は別のスタックを取る / 全部使用中なら待つ / 持ち主の run が終わっていれば古いロックを消す /
# release は自分のロックだけ消す、を確かめる。全ケースが通れば exit 0。
#   bash ci/bench/stacklock_test.sh
set -u
B=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin" "$T/locks" "$T/runs"
fails=0

# 偽 gh: contents API（GET / PUT（無ければ作る。あれば 422）/ DELETE）、branches、actions/runs/<id>（$T/runs/<id> の中身が status）
cat >"$T/bin/gh" <<'EOF'
#!/bin/bash
LOCKS=$FAKE_GH_DIR/locks RUNS=$FAKE_GH_DIR/runs
method=GET; path=; jq_expr=; f_sha=; f_content=
while [ $# -gt 0 ]; do
  case $1 in
    api) shift ;;
    -X) method=$2; shift 2 ;;
    -f) case ${2%%=*} in sha) f_sha=${2#*=} ;; content) f_content=${2#*=} ;; esac; shift 2 ;;
    --jq) jq_expr=$2; shift 2 ;;
    -H) shift 2 ;;
    *) path=$1; shift ;;
  esac
done
case $path in
  repos/*/branches/*) exit 0 ;;
  repos/*/actions/runs/*)
    id=${path##*/}
    status=$(cat "$RUNS/$id" 2>/dev/null || echo completed)
    jq -n --arg s "$status" '{status: $s}' | jq -r "${jq_expr:-.}"
    ;;
  repos/*/contents/*)
    name=${path##*/contents/}; name=${name%%\?*}
    file=$LOCKS/$name
    case $method in
      GET)
        [ -f "$file" ] || { echo "HTTP 404" >&2; exit 1; }
        jq -n --arg sha "sha-$(cksum <"$file" | cut -d' ' -f1)" --arg content "$(base64 <"$file" | tr -d '\n')" '{sha: $sha, content: $content}' | jq -r "${jq_expr:-.}"
        ;;
      PUT)
        # 原子的な作成: 既にあれば 422（GitHub と同じ）
        if [ -e "$file" ]; then echo "HTTP 422 sha wasn't supplied" >&2; exit 1; fi
        if ! (set -o noclobber; base64 -d <<<"$f_content" >"$file") 2>/dev/null; then echo "HTTP 422" >&2; exit 1; fi
        echo '{}'
        ;;
      DELETE)
        [ -f "$file" ] || { echo "HTTP 404" >&2; exit 1; }
        [ "$f_sha" = "sha-$(cksum <"$file" | cut -d' ' -f1)" ] || { echo "HTTP 409 sha mismatch" >&2; exit 1; }
        rm -f "$file"; echo '{}'
        ;;
    esac
    ;;
  *) echo "fake gh: unknown $method $path" >&2; exit 1 ;;
esac
EOF
chmod +x "$T/bin/gh"
export PATH="$T/bin:$PATH" FAKE_GH_DIR=$T GH_TOKEN=x GITHUB_REPOSITORY=o/r GITHUB_RUN_NUMBER=1 LABEL='#1'
export BENCH_REPLICA_STACKS="r1 r2"

claim() { # claim <run_id> <state_dir>
  mkdir -p "$2"
  echo in_progress >"$T/runs/$1"
  GITHUB_RUN_ID=$1 STATE_DIR=$2 timeout "${CLAIM_TIMEOUT:-20}" "$B/stacklock.sh" claim 2>"$2/err"
}
expect() { # expect <name> <got> <want>
  if [ "$2" = "$3" ]; then echo "ok   $1 ($2)"; else echo "FAIL $1: got '$2' want '$3'"; fails=$((fails + 1)); fi
}

got=$(claim 101 "$T/s101"); expect "1 本目は r1 を取る" "$got" r1
expect "STATE_DIR/stack に残す" "$(cat "$T/s101/stack")" r1
got=$(claim 102 "$T/s102"); expect "2 本目は r2 を取る" "$got" r2
got=$(CLAIM_TIMEOUT=3 claim 103 "$T/s103"); rc=$?
expect "3 本目は空くまで待つ（timeout 124）" "$rc:$got" "124:"
grep -q 'all replica stacks busy' "$T/s103/err" && echo "ok   待ちのメッセージ" || { echo "FAIL 待ちのメッセージ"; fails=$((fails + 1)); }

# 101 の run が終わった（runner が死んで release できなかった）→ 古いロックとして消して取る
echo completed >"$T/runs/101"
got=$(claim 104 "$T/s104"); expect "持ち主が completed のロックは消して取る" "$got" r1
expect "r1 のロックは 104 のもの" "$(jq -r .run_id <"$T/locks/r1.lock")" 104

# 同じ run がもう一度 claim しても同じスタック
got=$(claim 104 "$T/s104b"); expect "同じ run の再 claim は同じスタック" "$got" r1

# release: 自分のロックだけ消す
GITHUB_RUN_ID=102 STATE_DIR=$T/s102 "$B/stacklock.sh" release >/dev/null 2>&1
[ -e "$T/locks/r2.lock" ] && { echo "FAIL release r2"; fails=$((fails + 1)); } || echo "ok   release は自分のロックを消す"
mkdir -p "$T/s999" && echo r1 >"$T/s999/stack"
GITHUB_RUN_ID=999 STATE_DIR=$T/s999 "$B/stacklock.sh" release >/dev/null 2>&1
[ -e "$T/locks/r1.lock" ] && echo "ok   他人のロックは消さない" || { echo "FAIL 他人のロックを消した"; fails=$((fails + 1)); }
mkdir -p "$T/s000"
GITHUB_RUN_ID=0 STATE_DIR=$T/s000 "$B/stacklock.sh" release >/dev/null 2>&1 && echo "ok   取っていない run の release は何もしない" || { echo "FAIL release without stack"; fails=$((fails + 1)); }

# 空の BENCH_REPLICA_STACKS はエラー
BENCH_REPLICA_STACKS= GITHUB_RUN_ID=1 STATE_DIR=$T/s000 "$B/stacklock.sh" claim >/dev/null 2>&1 && { echo "FAIL empty stacks"; fails=$((fails + 1)); } || echo "ok   BENCH_REPLICA_STACKS が空なら exit 1"

if [ "$fails" = 0 ]; then echo "PASS: all stacklock.sh cases"; else echo "FAILED: $fails case(s)"; exit 1; fi
