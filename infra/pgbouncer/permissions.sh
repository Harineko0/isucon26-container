#!/bin/bash
# Run over SSH after rsync. Print the managed path only when its mode differs,
# so deploy.sh also restarts the service after a permission-only repair.
set -eu
dry=${1:-0}
dir=/usr/local/lib/isureal
binary=$dir/pgbouncer-1.26.1

if ! sudo test -f "$binary"; then
  [ "$dry" = 1 ] && exit 0 # The first dry run has no built binary yet.
  echo 'ERROR: PgBouncer binary is missing' >&2
  exit 1
fi

if [ "$(sudo stat -c %a "$dir")" != 755 ] ||
   [ "$(sudo stat -c %a "$binary")" != 755 ]; then
  if [ "$dry" != 1 ]; then
    sudo chmod 0755 "$dir" "$binary"
  fi
  echo usr/local/lib/isureal/pgbouncer-1.26.1
fi
