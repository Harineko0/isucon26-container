# 引き継ぎ: 「ISUCON26 App1 ベンチマーク調査・改善」セッションから

デプロイとベンチの実行権は「ISUCON26 ベンチマークスコア向上」セッションに移した（ユーザー判断、2026-10-31 11:57）。このセッションは以後デプロイ・ベンチをしない。

## スコアの推移（ポータル）

| ジョブ | スコア | 主な変更 |
|---|---:|---|
| #448 | 3465 | 無改修 |
| #1769 | 6965 | 索引、PG 設定、署名キャッシュ、画像 1 クエリ化 |
| #2064 | 13837 | 投稿・ログイン・登録を DB 関数化（1 往復）、SSE watcher、viewer context 1 クエリ |
| #2854 | 41688 | 関係系の DB 関数、GET の投機的セッション確認、FoF の索引化、users のテーブルロック撤去 |
| #3835 | 45157 | AVX-512 で署名 16 本同時計算（asia1 の CPU 67% → 解消） |
| #4212 | FAIL | asia2 の OOM（接続 3×64）→ プール 40、shared_buffers 768MB、work_mem 4MB、PG に Restart=on-failure |
| #4449 | 45091 | 投稿期間の開始 3s → 0.1s（通知 id を時刻順、一意索引を (window, user) に） |
| #5552 | 41905 | 書き込みのセッション確認を書き込み SQL に同居、feed の SQL 絞り込み、検索の前段絞り込み |

## 現在のボトルネック（#5552 時点）

- **asia2 (DB) が飽和**。asia は 222 人まで増えるがグランプリ通過 26 人。EU/US は 73/45 人で頭打ち。
- DB 時間の上位: 投稿画像の INSERT（平均 381KB、165ms/件）、利用者検索（f98c321 で trigram 化、未計測）、エクスポートの画像読み込み、画像取得の可否判定。
- asia の全エンドポイントで最大 6s 前後の詰まり → 接続プール(40)の枯渇か DB 飽和。f98c321 で `dbstats ...` を stderr(journal) に 5 秒ごとに出すようにした（`journalctl -u isureal-go | grep dbstats`）。

## サーバ上の状態（コード以外）

- asia2: `/etc/postgresql/16/main/conf.d/tuning.conf`（shared_buffers 768MB, work_mem 4MB, max_connections 200, pg_stat_statements, log_min_duration_statement=100）、`/etc/systemd/system/postgresql@16-main.service.d/restart.conf`。pg_trgm 拡張あり。
- asia1: `/home/isucon/imgcache/<先頭2文字>/<id>` に初期データの画像 1,826,950 枚（16GB）。`cmd/imgsync` で作成済み。アプリは閲覧可否を DB で確認したうえで本体をここから読む（不変なので initialize で消さなくてよい）。
- 3 台: nginx に LTSV ログ（reqtime 付き）、upstream keepalive、worker_connections 8192。`env.sh` に `ISUREAL_LOG_LEVEL=info`（guard は info の行だけ読むので BAN は効く）。
- envcheck は 4 台とも成功（11:56）。

## 注意

- `deploy.sh` はローカルの `app/webapp` を rsync して `GOEXPERIMENT=simd` でビルドする（署名の SIMD 版が有効になる）。サーバ上で直接編集すると、次の deploy.sh で上書きされる。
- DB 関数（`sql/004-functions.sql` ほか）を postgres ユーザーで作ると、`init.sh`（isureal ユーザー）の `CREATE OR REPLACE` が失敗する。所有者を isureal にする。
- 11:53 頃 initialize が `exit status 3` を返した（サーバと手元で SQL ファイルが食い違った間）。デプロイ後は必ず `./smoke.sh` で initialize から通しで確認する。

## 次に効きそうなこと

1. 投稿画像を DB に入れる負荷を減らす（WAL・IO）。例: 投稿元リージョンのディスクに保存し、他リージョンはルーター経由で取り寄せてキャッシュ。
2. DB の接続プールとメモリの再調整（`dbstats` を見て）。asia1 の待ちが多ければ asia1 だけプールを増やし、EU/US を減らす。
3. 署名の exp の丸めを 10s より大きくするとキャッシュが効く（ベンチが exp の幅を検査していないか要確認）。
4. 再起動試験（4 台再起動 → initialize → ベンチ 75% 以上、データ永続）はまだ実施していない。
