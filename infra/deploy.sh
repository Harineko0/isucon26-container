#!/bin/bash
# infra/<role>/ と infra/hosts/<host>/ をサーバの / に配り、変わったものだけ反映する。
#
#   infra/deploy.sh -n [host...]   # 変わるファイルを表示するだけ（既定は全台）
#   infra/deploy.sh [host...]
#
# 中身の差分は infra/pull.sh -n で見る。上書き前のファイルは
# サーバの /var/tmp/infra-backup/<時刻>/ に残る。
# PostgreSQL の再起動が必要な設定は reload しても効かないので、表示に従って手で再起動する。
set -eu
. "$(dirname "$0")/lib.sh"

DRY=
if [ "${1:-}" = -n ]; then DRY=1; shift; fi
[ -n "$DRY" ] || . "$(dirname "$0")/../ci/guard.sh"
HOSTS="${*:-$ALL_HOSTS}"
TS=$(date +%Y%m%d-%H%M%S)

# The versioned binary is self-contained. Only the asia1 service runs it.
if [ -z "$DRY" ] && [[ " $HOSTS " == *" asia1 "* ]] &&
    ! $SSH "$(host_addr asia1)" 'test -x /usr/local/lib/isureal/pgbouncer-1.26.1'; then
  docker buildx build --platform linux/amd64 \
    --output "type=local,dest=$INFRA_DIR/hosts/asia1/usr/local/lib/isureal" "$INFRA_DIR/pgbouncer"
fi

# -a は使わない（/ や /etc の所有者・権限が手元のものに置き換わる）。
# 既存ファイルの所有者と権限はそのまま、新規ファイルは root 所有になる。
RSYNC_OPTS=(-rlc --no-perms --no-owner --no-group --omit-dir-times
  --rsync-path="sudo rsync" --out-format=%n)

deploy_host() {
  local host=$1 addr role changed=
  addr=$(host_addr "$host")
  role=$(host_role "$host")
  for d in "$INFRA_DIR/$role" "$INFRA_DIR/hosts/$host"; do
    [ -d "$d" ] || continue
    changed+=$(rsync "${RSYNC_OPTS[@]}" ${DRY:+--dry-run} \
      --backup --backup-dir="/var/tmp/infra-backup/$TS" \
      -e "$SSH" "$d/" "$addr:/" | grep -v '/$' || true)$'\n'
  done
  # --no-perms preserves existing modes, including a root-owned 0700 directory
  # from a previous Docker export. Repair only the dedicated pool paths, and
  # treat a permission-only repair as a change so the failed service restarts.
  if [ "$host" = asia1 ]; then
    local pool_changed
    pool_changed=$($SSH "$addr" "bash -s -- ${DRY:-0}" \
      <"$INFRA_DIR/pgbouncer/permissions.sh") || {
      echo "[$host] ERROR: could not ensure PgBouncer permissions" >&2
      return 1
    }
    changed+="$pool_changed"$'\n'
  fi
  changed=$(echo "$changed" | sed '/^$/d' | sort -u)

  if [ -z "$changed" ]; then
    echo "[$host] no changes"
    return
  fi
  echo "$changed" | sed "s|^|[$host] /|"
  [ -z "$DRY" ] || return 0

  local cmds=()
  if grep -q '^etc/systemd/' <<<"$changed"; then
    cmds+=("sudo systemctl daemon-reload")
    grep -q '^etc/systemd/system/postgresql' <<<"$changed" &&
      cmds+=("echo 'NOTE: PostgreSQL の unit を変えた。効かせるには sudo systemctl restart postgresql@16-main'")
  fi
  if grep -Eq '^(etc/systemd/system/isureal-go.service$|etc/isureal/pgbouncer.ini$|usr/local/libexec/isureal-with-pool$|usr/local/lib/isureal/pgbouncer-)' <<<"$changed"; then
    cmds+=("sudo systemctl restart isureal-go && timeout 30 bash -c 'until systemctl is-active --quiet isureal-go && ss -Hltn \"sport = :8080\" | grep -q .; do sleep 1; done' || echo 'ERROR: isureal-go did not start'")
  fi
  grep -q '^etc/nginx/' <<<"$changed" &&
    cmds+=("sudo nginx -t -q && sudo systemctl reload nginx || echo 'ERROR: nginx -t failed; 旧設定のまま動作中。/var/tmp/infra-backup/$TS から戻す'")
  grep -q '^etc/sysctl.d/' <<<"$changed" &&
    cmds+=("sudo sysctl -q --system")
  grep -q '^etc/security/limits.d/' <<<"$changed" &&
    cmds+=("echo 'NOTE: limits.d は対象サービスの再起動か再ログインで効く'")
  if grep -q '^etc/postgresql/' <<<"$changed"; then
    cmds+=("sudo systemctl reload postgresql@16-main"
      "sudo -u postgres psql -XAtc \"SELECT 'ERROR: ' || sourcefile || ':' || sourceline || ' ' || error FROM pg_file_settings WHERE error IS NOT NULL\""
      "sudo -u postgres psql -XAtc \"SELECT 'NOTE: 再起動が必要 ' || name FROM pg_settings WHERE pending_restart\"")
  fi
  cmds+=("sudo /usr/local/bin/envcheck | grep -E '^  \"(passed|message)\"'")

  local script
  script=$(printf '%s\n' "${cmds[@]}")
  $SSH "$addr" "$script" 2>&1 | sed "s|^|[$host] |"
}

for host in $HOSTS; do
  deploy_host "$host" &
done
wait
