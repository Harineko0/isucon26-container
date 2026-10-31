# nginx /api/: keep uploads and image responses off temp files (#36)

`infra/app/etc/nginx/sites-available/isureal.conf`, `location /api/`:
`client_body_buffer_size 2m; proxy_buffer_size 16k; proxy_buffers 64 16k;`, and
`access_log ... buffer=256k flush=1s`. `/api/notifications` (SSE, `proxy_buffering off`)
and `client_max_body_size 32m` are unchanged.

## Evidence (2026-10-31 04:23 UTC, during a bench, read-only)

- `error.log` has 0 "buffered to a temporary file" lines on all 3 hosts, but that warning
  is `[warn]` and `error_log` uses the default level `error`, so it is never written.
  The temp directories show the spills instead:
  - `/var/lib/nginx/proxy` hash subdirectories touched in the last 15 min:
    asia1 572 / 951, europe1 302 / 782, america1 270 / 734.
  - `/var/lib/nginx/body` (no levels) mtime 04:23:51 on asia1, 04:23:49 on europe1, i.e.
    upload bodies were being written there during the bench.
- Defaults: `client_body_buffer_size` 16k, `proxy_buffers 8 4k|8k`. Responses > 64 KB in
  asia1 access.log: 1,045 `/api/images/:id` (avg 506 KB, max 893 KB),
  128 `/api/me/export` (avg 1.75 MB, max 5.5 MB). Uploads ~360 KB avg, ~1 MB max.

## Sizing

- 64 x 16k = 1 MiB per proxied response: every image fits; exports still overflow to a temp
  file (`proxy_max_temp_file_size` left at default, so no back-pressure on Go).
- Buffers are allocated per request only as needed; 200 concurrent image responses ~ 100 MB.
- Request bodies up to 2 MB stay in memory; larger ones spill as before.

## Bench plan / revert

Metric: CI `/bench` score, plus touched temp dirs after the bench
(`sudo find /var/lib/nginx/proxy -mindepth 2 -type d -mmin -5 | wc -l`, `stat /var/lib/nginx/body`).
Prediction: image temp-file writes go to ~0 (only exports remain); score flat to +1-2%
(saves a write+read of ~0.5 MB per image; CPU/IO few %). Revert if score drops beyond
run-to-run noise (~2-3%) or nginx memory grows enough to squeeze asia1.
