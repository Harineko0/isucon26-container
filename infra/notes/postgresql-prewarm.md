# asia2: pg_prewarm autoprewarm (#35) — needs a manual PostgreSQL restart

`infra/db/etc/postgresql/16/main/conf.d/tuning.conf`:
`shared_preload_libraries = 'pg_stat_statements,pg_prewarm'` (pg_stat_statements kept).
The line is edited in tuning.conf rather than a new conf.d file because conf.d files load
in name order and `tuning.conf` sorts after `NN-*.conf`, so a new file would be overridden.

**Until PostgreSQL is restarted this does nothing.** `infra/deploy.sh` reloads PG; the reload
ignores the changed value and prints `NOTE: 再起動が必要 shared_preload_libraries`.
Restart by hand when no bench is running: `sudo systemctl restart postgresql@16-main`, then
`sudo -u postgres psql -Atc "show shared_preload_libraries"` and check
`/var/log/postgresql/postgresql-16-main.log` for `autoprewarm`.
`/usr/lib/postgresql/16/lib/pg_prewarm.so` exists on asia2 (checked), so the restart will not
fail on a missing library. No `CREATE EXTENSION` is needed for autoprewarm.

## Why

- shared_buffers 768 MB, host RAM 3.79 GB. Non-image relations + indexes = 1.78 GB
  (notifications 314 MB, notifications_window_user 247 MB, realmojis 231 MB, notifications_pkey
  181 MB, posts 160 MB, ...). image_objects (most of the 16 GB DB) is mostly not cacheable anyway.
- The re-test reboots every host, which empties both shared_buffers and the OS page cache, then
  needs >= 75 % of the final score. `/api/initialize` (reset.sql) only deletes rows newer than the
  boundary and re-inserts relation tables; it does not re-read the hot indexes.
- autoprewarm (`pg_prewarm.autoprewarm = on`, `autoprewarm_interval = 300s`, defaults) dumps
  the buffer list to `$PGDATA/autoprewarm.blocks` and reloads up to 768 MB in block order at
  startup with a background worker, before / while the first requests arrive.

## Bench plan / revert

Not measurable by a normal `/bench`. Check at the restart rehearsal: time-to-steady-state and the
score of the first bench after a full reboot, with vs. without. Prediction: final score unchanged;
first post-reboot bench warmer (fewer `shared_blks_read` in pg_stat_statements early on).
Revert: if PG fails to start, remove `,pg_prewarm` and restart.
