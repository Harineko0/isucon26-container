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

通常の SQL は各リージョンから asia1 のプライベートアドレスの PgBouncer（6432）に入り、asia2 の PostgreSQL で実行される。プーラー側は 16 トランザクションを並行実行し、アプリ側は Asia 64、Europe・America 各 128 接続で往復を待てる。initialize の psql と投稿期間 watcher は元の接続先を使う。

初回の CI デプロイで `pgbouncer/Dockerfile` から静的バイナリをビルドする。asia1 の再起動時は `isureal-go.service` がディスクのバイナリを使い、DB から SCRAM 検証子を専用のランタイムディレクトリに読み込む。直結版に戻す場合はその版を通常の CI で配ると unit の差し替えでプーラーも停止する。
