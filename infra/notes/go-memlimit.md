# isureal-go: GOMEMLIMIT=2GiB (#39)

`infra/app/etc/systemd/system/isureal-go.service`: `Environment=GOMEMLIMIT=2GiB`.
GOGC stays at the default 100. Binary is go1.26.5 (GOMEMLIMIT needs >= 1.19).

## Evidence (asia1, 2026-10-31 04:25 UTC, end of a bench, read-only, pprof on 127.0.0.1:6060)

| | value |
|---|---|
| RSS | 1.64 GB (2.5 min after start) |
| HeapAlloc / HeapInuse | 956 MB / 965 MB |
| NextGC | 1.66 GB |
| Sys | 1.68 GB |
| NumGC | 58 |
| GCCPUFraction | 0.0069 (0.7 %) |
| MemAvailable (host) | 1.48 GB of 3.79 GB, no swap |

europe1 / america1: RSS ~300-340 MB at the same time. No OOM in dmesg, `NRestarts=0`.

## Reasoning

- GC CPU is 0.7 %, so `GOGC=200` would buy almost nothing and double heap headroom. Not set.
- The image cache keeps two 384 MB generations (`imageDataGenerationBytes`, up to 768 MB live).
  With GOGC=100 the heap target is 2 x live: a full cache + working set (~1.1 GB live) would
  target ~2.2 GB+, more than asia1 has available next to nginx and the page cache.
  An OOM kill mid-bench or during the re-test is a FAIL risk.
- GOMEMLIMIT=2GiB is above the observed peak (Sys 1.68 GB), so today's GC frequency does not
  change; it only makes the GC work harder when the heap approaches 2 GiB. Live heap (~1.1 GB
  worst case) is far below the limit, so no GC death spiral. It is a soft limit: it never
  kills the process. Same value on europe1/america1 (harmless at ~300 MB).

## Bench plan / revert

Metric: CI `/bench` score; after the bench, `curl -s 127.0.0.1:6060/debug/pprof/heap?debug=1`
on asia1 (`GCCPUFraction`, `NumGC`, `Sys`) and `ps -o rss -C isureal`.
Prediction: score unchanged (within noise), GCCPUFraction stays < 2 %, RSS <= ~2.1 GB.
Revert (or raise to 2.5GiB) if GCCPUFraction > 5 % or NumGC jumps several-fold vs. this baseline.
