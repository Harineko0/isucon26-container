#!/bin/bash
# freeze.sh の単体テスト（#245）。全ケースが通れば exit 0。
#   bash ci/bench/freeze_test.sh
# bench.yml と同じく freeze.sh を作業ツリーの外（$RUNNER_TEMP 相当）に置き、stage.sh はスタブを
# $GITHUB_WORKSPACE/ci/bench/ に置く（本物の stage.sh は PR コメントと status を書き換えるので使わない）。
set -u
SRC=$(cd "$(dirname "$0")" && pwd)/freeze.sh
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/ws/ci/bench" "$T/runner"
cp "$SRC" "$T/runner/freeze.sh"
cat >"$T/ws/ci/bench/stage.sh" <<'EOF'
#!/bin/bash
echo "$1${2:+:$2}" >"$STATE_DIR/stage"
EOF
chmod +x "$T/runner/freeze.sh" "$T/ws/ci/bench/stage.sh"
export STATE_DIR=$T GITHUB_WORKSPACE=$T/ws FREEZE_CHECK=$T/runner/freeze.sh

PAST=2000-01-01T00:00:00+09:00
FUTURE=2099-01-01T00:00:00+09:00
fails=0

# check <name> <want_rc> <want_stage_glob> <FREEZE_AT> <LABEL>（stage が無いことを期待するときは glob に "" ）
check() {
  local name=$1 want_rc=$2 want_stage=$3 rc stage
  rm -f "$T/stage"
  FREEZE_AT=$4 LABEL=$5 "$FREEZE_CHECK" >/dev/null 2>&1
  rc=$?
  stage=$(cat "$T/stage" 2>/dev/null || true)
  local stage_ok=
  if [ -z "$want_stage" ]; then
    [ -z "$stage" ] && stage_ok=1
  else
    # shellcheck disable=SC2053 # want_stage は glob
    [[ $stage == $want_stage ]] && stage_ok=1
  fi
  if [ "$rc" = "$want_rc" ] && [ -n "$stage_ok" ]; then
    echo "ok   $name (rc=$rc stage=${stage:-none})"
  else
    echo "FAIL $name: rc=$rc (want $want_rc) stage=${stage:-none} (want ${want_stage:-none})"
    fails=$((fails + 1))
  fi
}

check "past + #12 -> rejected" 1 'rejected:*' "$PAST" '#12'
check "past + main -> allowed" 0 '' "$PAST" main
check "future + #12 -> allowed" 0 '' "$FUTURE" '#12'
check "future + main -> allowed" 0 '' "$FUTURE" main

# run.sh と同じ呼び方: set -euo pipefail + ERR trap の下で `|| exit 1` しても stage は rejected:* のまま
rm -f "$T/stage"
FREEZE_AT=$PAST LABEL='#12' bash -c '
  set -euo pipefail
  trap "echo failed:freeze >\"\$STATE_DIR/stage\"" ERR
  "$FREEZE_CHECK" || exit 1
  echo reached-deploy >"$STATE_DIR/stage"
' >/dev/null 2>&1
rc=$?
stage=$(cat "$T/stage" 2>/dev/null || true)
if [ "$rc" = 1 ] && [[ $stage == rejected:* ]]; then
  echo "ok   run.sh-style || exit 1 keeps rejected (rc=$rc stage=$stage)"
else
  echo "FAIL run.sh-style || exit 1: rc=$rc (want 1) stage=${stage:-none} (want rejected:*)"
  fails=$((fails + 1))
fi

if [ "$fails" = 0 ]; then
  echo "PASS: all freeze.sh cases"
else
  echo "FAILED: $fails case(s)"
  exit 1
fi
