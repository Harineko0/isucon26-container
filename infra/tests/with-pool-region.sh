#!/bin/bash
# #68: europe1 / america1 は asia1 の PgBouncer を通さず asia2 に直接つなぐ (ISUREAL_APP_DB_POOL を渡さない)。
# asia は従来どおり PgBouncer を使う。isureal-with-pool をスタブのアプリで動かして確かめる。
# 使い方: infra/tests/with-pool-region.sh
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
script=app/usr/local/libexec/isureal-with-pool
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
printf '#!/bin/bash\necho "pool=${ISUREAL_APP_DB_POOL-unset}"\n' > "$work/app"
chmod +x "$work/app"
fail=0
for region in europe america; do
	got=$(ISUREAL_REGION=$region ISUREAL_APP_BIN="$work/app" bash "$script")
	if [[ $got == "pool=unset" ]]; then
		echo "ok   $region: app connects to PostgreSQL directly"
	else
		echo "FAIL $region: $got (want pool=unset)"
		fail=1
	fi
done
# asia は PgBouncer を起動してから ISUREAL_APP_DB_POOL を渡す (起動には DB が要るので、ここでは順序だけ見る)。
export_line=$(grep -n '^export ISUREAL_APP_DB_POOL=192.168.1.10:6432$' "$script" | cut -d: -f1)
branch_end=$(grep -n '^fi$' "$script" | head -1 | cut -d: -f1)
pool_line=$(grep -n 'pgbouncer-1.26.1 /etc/isureal/pgbouncer.ini &' "$script" | cut -d: -f1)
if [[ -n $export_line && $export_line -gt $branch_end && $export_line -lt $pool_line ]]; then
	echo "ok   asia: ISUREAL_APP_DB_POOL is exported after the europe/america branch and before PgBouncer starts"
else
	echo "FAIL asia: export=$export_line branch_end=$branch_end pgbouncer=$pool_line"
	fail=1
fi
bash -n "$script" && echo "ok   syntax"
exit $fail
