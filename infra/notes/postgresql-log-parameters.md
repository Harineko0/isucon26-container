# PostgreSQL slow log: bind parameters cut to 64 bytes (#23)

`infra/db/etc/postgresql/16/main/conf.d/90-log-parameters.conf` sets
`log_parameter_max_length = 64`. `infra/deploy.sh` reloads PostgreSQL after a
change under `etc/postgresql/`. The setting has context `superuser`, so the
reload applies it without a restart.

## Evidence (asia2, 2026-10-31 04:07 UTC, read-only)

- `log_min_duration_statement = 100` (tuning.conf), `log_parameter_max_length = -1` (default),
  `log_parameter_max_length_on_error = 0` (default), `logging_collector = off`
  (Debian writes stderr to `/var/log/postgresql/postgresql-16-main.log`).
- That log file was 6,192,092,127 bytes (6.2 GB). It had been 4.1 GB a few hours earlier.
- In the last 2 GB of the file, 6,407 `DETAIL:  parameters:` lines took 1,991,596,991 bytes.
  2,056 of them were longer than 10 kB and took 1,990,777,312 bytes, about 970 kB per line.
  Those lines are the post INSERT, with `$3 = '\xffd8ffe0...'`, a whole JPEG in hex.
  So 99.96% of the log is image parameters.
- Disk: `/` 58 GB, 34 GB used, 24 GB free (59%).

## Why 64 rather than 0

The team reads the slow log. With 64, UUID and timestamp parameters stay whole,
so a logged query can still be reproduced with EXPLAIN. An image parameter
becomes 64 characters plus `...`. The value is still converted to hex before it
is cut. That costs CPU on the 2-vCPU DB, but only for statements already slower
than 100 ms, and the disk write is gone. If profiling shows that the conversion
matters, set the value to 0, which logs no parameters.

## The existing 6.2 GB file (for a human; not done by CI)

Disk space is not urgent (24 GB free). The logrotate config
(`/etc/logrotate.d/postgresql-common`) runs weekly with `copytruncate` and
`compress`. Running it now would copy and gzip 6 GB, which costs heavy I/O.
If the space is needed, run this between benchmarks, after saving any part the
team still wants:

    sudo truncate -s 0 /var/log/postgresql/postgresql-16-main.log

PostgreSQL keeps writing to the same file descriptor, which is opened with
O_APPEND by pg_ctlcluster, so truncation is safe.

# journald rate limit and isureal-guard (#38): no change needed

Checked read-only on asia1, europe1 and america1 on 2026-10-31 at 04:07 UTC.
The current boot started at 01:08 UTC and covers all benchmarks run today.

- The journal has no `Suppressed N messages from isureal-*` lines.
  `journalctl -b -g uppressed` returned only `kauditd_printk_skb ... callbacks suppressed` at boot.
- journald runs with defaults (`RateLimitIntervalSec=30s`, `RateLimitBurst=10000`).
  systemd 255 raises the burst according to free disk space; with 18 GB free it is about 4.5 times higher.
  isureal-go has no per-unit override (`LogRateLimitIntervalUSec=0`, which means the journald default).
- The busiest 30 s window of `isureal-*` on asia1 had 4,921 lines.
  That is under half of the unscaled limit.
- isureal-guard reads `GUARD_JOURNAL_UNIT=isureal-*.service` and is working.
  On asia1 it reported `action_logs=118845 failures=23270 bans=4110`.
- Journal disk use is 24-72 MB on each server.

Revisit if the score multiplies, or if `journalctl -u systemd-journald | grep -i suppressed`
ever shows isureal-go. The fix would then be a journald drop-in at
`infra/app/etc/systemd/journald.conf.d/`. `infra/deploy.sh` does not restart journald today,
so that change would also need `systemctl restart systemd-journald` added for that path.
