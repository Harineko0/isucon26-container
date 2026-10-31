# ISUCON2026 (ISUReal) 改善方針

コード参照は `app/webapp/rust/src/*.rs`(Go 版とファイル名・SQL・ロック順が同一。`xxx.rs` ⇔ `xxx.go`)。

## 0. 前提

| サーバ | 種別 | 役割 | 備考 |
|---|---|---|---|
| asia1 (192.168.1.10) | c5.large 2vCPU | nginx + app | 倍率 1.0 |
| europe1 (192.168.2.10) | c7i.large 2vCPU | nginx + app | **倍率 1.5**、DB まで遅延+帯域制限 |
| america1 (192.168.3.10) | c7i.large 2vCPU | nginx + app | **倍率 1.5**、DB まで遅延+帯域制限 |
| asia2 (192.168.4.10) | c5.large 2vCPU | PostgreSQL 16 | 全 app が共有 |

- スコアの大口: 投稿後の友達フィード検証 10/20 点、エクスポート 20、RealMoji 5、退会 5、コメント 3、Memories 月 3。閲覧系はユーザー×投稿期間ごとに上限あり → **投稿期間 1 回の「通知→投稿→フィード確認」サイクルを速く回すこと**が本筋。
- 制約: 書き込み後の読み取りは**リージョンをまたいでも即反映**(非同期レプリカ・TTL キャッシュは不可)。画像の削除反映だけ 5 秒猶予。
- 追試: 全台再起動 → 最終スコアの 75% 超 + データ永続。キャッシュが冷えた状態でも性能が出ること。
- 最終スコア = 最後のベンチ。**17:30 以降は安定版でしか回さない**。

### 着手前に知っておくべき罠

1. **Go のソースがリポジトリにない**。`app/.gitignore` の `go/` が `webapp/go/` まで除外している。`/go/` に直してサーバーから `~/webapp/go` を取得しコミットする。
2. サーバーの `/home/isucon` に `AGENTS.md` / `.claude/CLAUDE.md` / `.codex/AGENTS.md` / `.gemini/GEMINI.md` 等(全部同一内容)があり、「本番環境なので sudo・再起動・設定変更・DB 書き込み禁止、sleep を挟んでゆっくり操作」と指示している。サーバー上で AI エージェントを動かすとほぼ何もできない。**エージェントはローカルでコードを書き、デプロイはスクリプト or 人間**が基本。これらのファイルの扱いはチームで判断する(マニュアル上は保護対象外)。
3. `001-schema.sql` に「画像は DB に置け、決して変更するな」というコメントがある。マニュアルは保存方式の変更を許可しているが、**どのリージョンのサーバーからでも同じ画像を返せる**ことは必須(過去障害の原因)。
4. `POST /api/initialize` はルーター経由で 1 台(たぶん asia1)にしか来ない。プロセス内キャッシュを入れたら、**他 2 リージョンのキャッシュもルーター経由で消す**処理を initialize に入れる。
5. `initialize` の応答の `agent/harness/model` は、エージェントを使うなら実態に合わせて書き換える(申告用、得点には無関係)。
6. `ip_bans` に INSERT するコードが app に無い。action log(stdout の JSON、`abuse` フィールド付き)を外部プロセスが読んで BAN している可能性がある。**ログレベル・ログ形式は書き込み元を確認するまで変えない**。

## 1. 最初の 60 分(全員)

1. 初回ベンチを投げる(EBS 初期化完了を確認してから)。並行して以下。
2. リポジトリ整備: `.gitignore` 修正、Go ソース・nginx/postgres/systemd 設定・`env.sh` を 4 台分取得してコミット、ホスト別 `deploy.sh`(ビルド→配布→restart→ログローテート)。
3. 計測を入れる: nginx に alp 用 LTSV ログ、asia2 に `pg_stat_statements` + `log_min_duration_statement`、Go に pprof。
4. **リージョン間の RTT と帯域を測る**(europe1/america1 → asia2 の ping と、`psql -c 'select 1'` 往復時間、iperf 相当)。以降の設計判断はこの数値次第。
5. DB サイズ確認(`image_objects` の総量、ユーザー数、投稿数)。

## 2. コードから見えたボトルネック(要計測で確定)

| # | 箇所 | 問題 | 対策案 | 影響 |
|---|---|---|---|---|
| 1 | `image_signature.rs` | 署名 1 つに HMAC-SHA256 を **1000 回反復**。フィード 1 回で投稿画像+著者アイコン × 50 件 = 最大 **10 万 HMAC**。画像取得の検証でも毎回 1000 回 | `exp` を 10 秒程度のバケットに丸め `(image_id, exp)` で署名を再利用。検証結果も `(image_id, exp, sig)` で LRU。署名鍵(`secret_key_hash`)は不変 | app CPU 大 |
| 2 | `session.rs` 認証 | `login_sessions.token_hash` に索引なし → **全 API で seq scan**。`users.username` も索引なし(login/register) | 索引追加 | DB 大 |
| 3 | `feed_api.rs` / `post.rs` | アクティブな投稿期間の**全投稿 × 著者を毎回全件取得**して、権限判定はアプリ側。viewer context に直列で 4〜5 クエリ。`friendships(user_high_id)` 索引なし | 著者候補(友達/FoF/フォロー/public)で SQL 側を絞り LIMIT。viewer context を 1 クエリに。索引追加 | DB 大・RTT |
| 4 | `notification_api.rs` SSE | **接続ごとに 500ms 間隔で 2 クエリ**のポーリング(EU/US はクエリごとにリージョン間往復) | プロセスに watcher を 1 本置き、`posting_windows` を短い間隔で監視 → 新しい投稿期間の通知をまとめて 1 クエリ取得し、接続中のクライアントに配る | DB 大・通知遅延 |
| 5 | `image_api.rs` | bytea を毎回 asia2 から取得(EU/US は帯域制限越え)。可否判定のサブクエリが `posts.image_id` / `realmojis.image_id` 索引なし。`image_objects` は `(id, created_at)` のパーティションなので id だけだと全パーティションを探索 | 画像本体は不変なので**各リージョンのローカルにキャッシュ**(メモリ or ディスク + nginx 配信)。可否判定だけ DB(削除は 5 秒猶予があるので短期キャッシュも可)。索引追加 | 帯域・DB |
| 6 | 全般(EU/US) | 1 リクエストで BEGIN、`FOR NO KEY UPDATE`、SELECT…、COMMIT が全部リージョン間往復 | クエリを CTE で束ねる、不要なロックや再取得(`get_post` / `get_user` の取り直し)を削る | EU/US のレイテンシ |
| 7 | `export_api.rs`(20 点) | 全投稿画像を DB から読んで zip | #5 のローカル画像キャッシュで軽くなる | 帯域 |
| 8 | `user_read_api.rs` 検索 | アクティブ全ユーザーを取得してアプリで case-fold 部分一致 | 正規化済みの列 + `pg_trgm`、またはメモリ索引(更新の反映に注意) | DB 中 |
| 9 | `post_api.rs` / `realmoji_api.rs` | JPEG を全デコード(最大 4096²)して検証 | 計測してから。不正 JPEG の 400 判定は維持する | app CPU 中 |
| 10 | register / 投稿期間開始 | `LOCK TABLE users IN SHARE ROW EXCLUSIVE` で users の UPDATE を全部止める | username に UNIQUE 索引を張ればロック不要 | 待ち 中 |
| 11 | その他の索引 | `follows(followee_id)`、`block_relations(blocked_id)`、`friend_requests(recipient_id,status)` / `(requester_id,status)`、`login_sessions(user_id)`、`administrator_sessions(token_hash)`、`comments`/`realmojis` の `(post_id, created_at)` | まとめて追加 | DB |

## 3. リージョン間の設計(計測後に決める)

採点倍率は EU/US が 1.5 倍なのに、どちらも DB への往復が遅い。段階を踏む。

- **A. 往復の削減 + 不変データのリージョン内キャッシュ**(安全。最初は全員これ)
  不変: 画像本体、`secret_key_hash`/署名鍵、投稿の不変列(author/image/window/posted_at/location)、投稿期間(admin の開始時だけ変わる → watcher で同期)。
- **B. 可変データのキャッシュ + 書き込み時に他リージョンへ同期無効化**
  書き込み API がルーター経由で他リージョンに無効化を送り、成功してから応答する。書き込みに RTT 分の遅延が乗るが、読みはローカル。対象はユーザー情報、友達・ブロック関係などに絞る。
- **C. PostgreSQL のレプリカを europe1/america1 に置き、`synchronous_commit = remote_apply`**
  読みは全部ローカル、整合性は保たれる。書き込みは最も遠いリージョンの RTT 分遅くなり、画像を含む WAL が帯域を食う。構築コストも大きい。RTT・帯域・書き込み比率を見て、昼過ぎまでに採否を決める。**非同期レプリカは read-after-write 違反なので不可**。

耐久性を下げる設定(`fsync=off`、`synchronous_commit=off` など)は追試の永続性チェックと相性が悪いので採らない。

## 4. 並列開発の分担(ファイル所有で衝突を避ける)

| 担当 | 範囲(Go ファイル) | 最初のタスク |
|---|---|---|
| **A: インフラ・計測・統合** | nginx/postgres/systemd 設定、deploy スクリプト、`initialize`、ベンチの実行権 | §1 の整備、RTT/帯域の計測、PG のメモリ設定、§3 の判断材料づくり |
| **B: DB・認証** | `sql/`(索引ファイル)、`session.go`、`user.go`、`user_api.go`、`admin_session.go`、`user_read_api.go` | #2 #10 #11 の索引、login/register の改善、検索 |
| **C: 画像** | `image_signature.go`、`image_api.go`、`export_api.go`、`profile_image_api.go` | #1 署名キャッシュ → #5 ローカル画像キャッシュ → #7 |
| **D: フィード・閲覧** | `feed_api.go`、`post.go`、`post_api.go`、`memory_api.go`、`comment*.go`、`realmoji*.go` | #3 フィードと viewer context、#6 往復削減 |
| **E: 通知・投稿期間** | `notification_api.go`、`admin_posting_window_api.go`、`posting_window*.go` | #4 SSE watcher、投稿期間のメモリ保持 |

ルール:
- 共有関数のシグネチャは変えない(例: `signedImageURL(user, imageID)` は C が中身だけ変え、D はそのまま呼ぶ)。
- 索引は `sql/` の冪等なファイル(`CREATE INDEX IF NOT EXISTS`)に追記し、`init.sh` から reset 前に流す。B がレビューする。
- **ベンチとデプロイは CI だけ**（[ci/README.md](ci/README.md)）。各自が PR を open すると（測り直しは `/bench` コメント）、CI が到着順に 1 本ずつデプロイしてベンチし、main の基準値との差を PR にコメントする。効いた PR から main にマージし、ときどき main を回して基準値を更新する。
- ローカルでは既存のテスト(`make test`)と簡単なスモークテストで仕様の後退を防ぐ。
- 新しく入れたキャッシュは、必ず initialize で全リージョン分を消す。

## 5. タイムライン

| 時刻 | 目標 |
|---|---|
| 10:00–11:00 | §1。初回スコア記録。 |
| 11:00–13:00 | 索引(#2 #11)、署名キャッシュ(#1)、SSE watcher(#4)、フィードのクエリ(#3)。1 つ入れるたびにベンチ。 |
| 13:00–15:30 | 往復削減(#6)、画像のローカルキャッシュ(#5 #7)、§3 の B/C の採否を決めて着手。 |
| 15:30–16:30 | 次のボトルネックを潰す。PG と nginx の調整、ワーカー数・接続プール。 |
| 16:30–17:15 | 機能凍結。**4 台再起動テスト**(データ永続、冷えたキャッシュでも 75% 以上出るか)、`envcheck`。 |
| 17:15–18:00 | 安定版だけでベンチ。最後の 1 本は必ず PASS するもの。デバッグ用ログ・pprof は外すか残すかを揃える。 |
