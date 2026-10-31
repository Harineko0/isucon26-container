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
