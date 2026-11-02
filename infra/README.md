# infra: /home 以外のサーバ設定

サーバの `/` からの相対パスでファイルを置く。ここにあるファイルだけを管理し、それ以外の `/etc` には触らない。

| ディレクトリ | 配り先 |
|---|---|
| `app/` | asia1 / europe1 / america1（共通） |
| `db/` | asia2 |
| `hosts/<host>/` | そのホストだけ（`app/` `db/` の同じパスより優先） |
| `notes/` | 配らない。設定変更の経緯メモ |

反映（`infra/deploy.sh` の実行）は CI だけが行う。PR に `/bench` とコメントする（[ci/README.md](../ci/README.md)）。手元では `-n` の差分表示と `pull.sh` だけを使う。

```bash
infra/pull.sh -n                     # リポジトリとサーバの差分を見る（全台）
infra/deploy.sh -n                   # 配ると変わるファイルを見る
infra/deploy.sh                      # 配って反映（nginx reload, daemon-reload, PG reload, envcheck）
infra/deploy.sh asia2                # 1 台だけ
infra/pull.sh asia1                  # サーバで直接直した内容を取り込む
infra/pull.sh asia2 /etc/foo.conf    # 新しいファイルを管理対象に加える
```

- 上書き前のファイルはサーバの `/var/tmp/infra-backup/<時刻>/` に残る。
- `isureal-go.service` を変えると isureal-go を再起動する。PostgreSQL の再起動が必要な設定（`shared_buffers` など）と PG の unit は表示が出るだけなので、ベンチが走っていないときに手で再起動する。
- `isucon-register.service`、`isureal-guard.service`（主催者の BAN 処理）、envcheck 関連ファイルはここに置かない。
- `fsync` / `synchronous_commit` など耐久性を下げる設定は入れない（追試で失格）。

## アプリの DB 接続

各アプリはリージョン間のルーターを通る通常の経路で、asia2 のプライベートアドレスの PgBouncer（192.168.4.10:6432）に接続する。PgBouncer から PostgreSQL までは同じホストの loopback を使う。フィード・画像などの読み取りは同じ DB の別名 `isureal_reads` に接続し、専用の 8 バックエンドを使う。書き込み、検索用データの更新を伴うユーザー検索、その他の処理は 16 バックエンドを使う。アプリ側の接続上限は通常処理が各 48、読み取りが Asia 64、Europe・America 各 80 で、合計は 368。initialize の psql と投稿期間 watcher は元の接続先を使う。

CI は先に asia2 にプーラーを配り、アプリと同じ資格情報で SELECT が通ってから 3 台のアプリを更新する。初回は `pgbouncer/Dockerfile` から静的バイナリをビルドする。`isureal-db-pool.service` は起動のたびに PostgreSQL から SCRAM 検証子を専用のランタイムディレクトリへ読み込む。サービスは OS の起動時にも立ち上がり、DB の準備に失敗した場合は systemd が再試行する。

プーラー用の `/usr/local/lib/isureal` とバイナリは CI が `0755` にそろえる。以前の asia1 版へ戻す場合は、CI で全アプリの接続先を戻してから asia2 の `isureal-db-pool.service` を停止し、自動起動を解除する。

asia1 の JPEG 検証枠が埋まったときは、64 KiB〜2 MiB の画像を同じリージョンの asia2 にも振り分ける。CI が同じコミットから検証器をビルドし、`isureal-jpeg-worker.service` として private IP の 6433 番で起動する。処理は 1 CPU に制限し、PostgreSQL より低い優先度で動かす。通信や検証サービスに障害があればアプリ内で検証する。画像の原本は受付リージョンで永続化する。
