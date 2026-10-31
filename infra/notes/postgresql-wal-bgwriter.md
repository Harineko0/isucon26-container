# asia2: bgwriter / WAL writer / checkpoint distance (#81 #76)

`infra/db/etc/postgresql/16/main/conf.d/tuning.conf`. Every setting here has context
`sighup` or `superuser`, so `infra/deploy.sh`'s reload applies it (checked in
`pg_settings.context`, read-only, 2026-10-31 05:03 UTC). Nothing restart-only is in the
file; see "Restart-only (for a human)" at the end.

| setting | before | after | context |
|---|---|---|---|
| `bgwriter_delay` | 200ms | 20ms | sighup |
| `bgwriter_lru_maxpages` | 100 | 1000 | sighup |
| `bgwriter_lru_multiplier` | 2.0 | 4.0 | sighup |
| `wal_writer_delay` | 200ms | 10ms | sighup |
| `max_wal_size` | 4GB | 8GB | sighup |
| `track_wal_io_timing` | off | on | superuser |

Unchanged on purpose: `fsync`, `synchronous_commit`, `full_page_writes` (durability; re-test),
`wal_compression` (only compresses full-page images, which are ~5 % of WAL here and are
JPEG pages that do not compress; the DB is CPU-bound), `commit_delay` (adds 1 ms to every
commit; `IO:WALSync` is only 1.4 % of samples), `checkpoint_completion_target` (0.9),
`checkpoint_timeout` (15 min), `log_lock_waits` (the 6.6 GB log; the extend waits average
0.4 s and would not reach the 1 s `deadlock_timeout` threshold anyway).

## Evidence (asia2, read-only, 2026-10-31)

Baseline = bench of `main` e55d7ee, CI run 41170358294 (score 53,172), `measure.sh` output.

- Wait events, 1,216 samples of active backends: CPU 40 %, **Lock:extend 28 %**,
  LWLock:WALWrite 5.4 %, LockManager 5.3 %, IO:DataFileRead 4.9 %, Lock:transactionid 4.0 %,
  IO:WALSync 1.4 %, IO:WALWrite 0.2 %, **IO:DataFileExtend 0.1 %**.
- `pg_stat_statements` for that run: `INSERT INTO image_objects (...)` inside
  `isureal_create_post` = 1,317 calls, **mean 390 ms**, 514 s total (#81 measured 166 ms at
  pool 40; the asia pool is 80 since #90, so more writers queue on the same lock).
  The last 500 images average 456 kB (max 923 kB) = ~230 TOAST chunks each.
- `pg_stat_io` client backend: extends 2,511,332 in 57.2 s = **23 µs per extend**. The file
  extension is cheap; the 390 ms is the queue for the relation extension lock of the TOAST
  table (`pg_toast_16604`), 160 backends on 2 vCPUs (vmstat during the bench: r=20,
  us 63 % sy 10 % id 22 % wa 5 %, bo 17 MB/s). Settings cannot shorten that queue; PR #100
  (8 hash partitions) and #33/#80 (images out of the DB) do.
- bgwriter is saturated: `pg_stat_bgwriter` since 02:32 UTC: buffers_backend 2,791,160 vs
  buffers_clean 676,035 vs buffers_checkpoint 127,413; **maxwritten_clean 4,821** (the
  bgwriter stopped at its 100-page cap 4,821 times). In the 6 minutes around one bench:
  +70k backend writes, +67k bgwriter writes, +524 maxwritten_clean. With 200 ms × 100 pages
  the bgwriter can clean at most 4 MB/s; the bench dirties 17 MB/s.
- WAL buffers overflow once per image: `pg_stat_wal` wal_buffers_full 94,997 of 293,541
  WAL writes (32 %); +8,070 in one bench. The WAL writer wakes every 200 ms, so backends
  write the 16 MB ring themselves and queue on WALWriteLock (the 5.4 %).
- Checkpoints: `pg_stat_bgwriter` checkpoints_req 10 vs checkpoints_timed 4. The log shows
  every checkpoint as `checkpoint starting: wal`, distance 2,187–2,196 MB (= 4 GB / 1.9),
  the next one 11–25 s after the previous `complete`; each writes only 2.5k–5.4k buffers but
  is paced over 341–725 s. wal_fpi 180,960 × 8 kB ≈ 1.4 GB of 23.4 GB WAL ≈ 13k FPIs
  (~100 MB) per checkpoint. Disk: `/` 58 GB, 23 GB free, pg_wal 4.0 GB, PG log 6.6 GB.

## Mechanism and prediction (pre-registered)

1. bgwriter 20 ms / 1000 pages / ×4: the cleaner runs ahead of the clock sweep, so a backend
   that needs a victim buffer (every TOAST page it extends: `ExtendBufferedRelBy` takes
   victims before the extension lock) finds a clean one instead of writing a dirty page and
   flushing WAL to its LSN first. Prediction: buffers_backend per bench drops below
   buffers_clean (now ~1:1); LWLock:WALWrite share drops. Cost: the bgwriter wakes 50×/s on
   a 2-vCPU host (microseconds each).
2. `wal_writer_delay` 10 ms: the WAL writer drains completed WAL pages 100×/s, so the 16 MB
   `wal_buffers` ring (fills in ~0.7 s at 22 MB/s) is rarely full and backends stop doing
   their own WAL writes. It also fsyncs at most every 10 ms / 1 MB (`wal_writer_flush_after`),
   so commits find WAL already flushed more often. Prediction: wal_buffers_full per bench
   8k → under 2k; LWLock:WALWrite + IO:WALSync share 7 % → ~3 %. This substitutes for the
   restart-only `wal_buffers` increase.
3. `max_wal_size` 8 GB: checkpoint distance 2.2 GB → 4.3 GB, so one checkpoint per two
   benches instead of one per bench and ~13k fewer FPIs (~100 MB WAL, ~5 %) per bench.
   Prediction: checkpoints_req stops growing every run. Cost: pg_wal up to ~8–9 GB (23 GB
   free); crash recovery replays up to 8 GB, but the re-test's normal restart does a shutdown
   checkpoint, so no effect there. Durability settings untouched.
4. `track_wal_io_timing`: makes `pg_stat_wal.wal_write_time` / `wal_sync_time` non-zero for
   the next measurement (two `clock_gettime` per WAL write; `track_io_timing` is already on).

Metrics for the bench after this PR (計測 section of the PR comment and the run log):
`Lock:extend` share, `LWLock:WALWrite` + `IO:WALSync` share, `INSERT INTO image_objects`
mean ms, score. Read-only on asia2 afterwards: `select * from pg_stat_wal`,
`select * from pg_stat_bgwriter`.

Expected score effect: small and positive (the WAL-side waits are ~7 % of DB samples; the
28 % extend queue is not addressed by settings). Revert the whole file change if the bench
is FAIL, if the score drops more than the run-to-run noise (~±3 %), or if
`maxwritten_clean` keeps growing at the same rate (then the bgwriter cap was not the limit).

## Restart-only (for a human, not done by CI)

`wal_buffers` (context `postmaster`, now 16 MB). 64 MB would remove most wal_buffers_full
events even without change 2. It needs `sudo systemctl restart postgresql@16-main` on asia2
while no bench is running, and `infra/deploy.sh` fails the CI run if the value in the file
differs from the running one (`pg_file_settings` error), so put it in the file only in the
same change as the restart. With change 2 in place, measure first; it may not be needed.
`shared_buffers` stays 768 MB (OOM history, b1958ba).
