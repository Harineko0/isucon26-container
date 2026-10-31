# PostgreSQL performance configuration

`99-disable-jit.conf` is installed on asia2 at
`/etc/postgresql/16/main/conf.d/99-disable-jit.conf`.
The main configuration already includes `conf.d`; apply changes with
`SELECT pg_reload_conf()` as a PostgreSQL administrator. A restart is unnecessary.

On 2026-10-31, the running feed SQL had accumulated 153,270 ms of execution time,
including 75,517 ms of JIT generation, optimization and emission. Its short API
queries repeatedly paid the compilation cost. After `jit = off`, another 5,612
calls added zero JIT functions or compilation time. The existing application
connections received the change, and all three application binaries stayed at
SHA-256 `84f780bcfd493367893976545a380c995b7c38cb03f1d2b1aebd808cb4f3141c`.

`pg_settings` confirmed `jit=off`, `fsync=on`, `synchronous_commit=on`, and no
pending restart. `pg_file_settings` reported no configuration errors.

This setting is persistent across initialization and PostgreSQL restarts. The
four-server reboot and competition recheck have not been performed here.

PostgreSQL explains the compilation overhead for short queries in
[When to JIT?](https://www.postgresql.org/docs/16/jit-decision.html).
