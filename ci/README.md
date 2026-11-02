# CI ベンチ

デプロイとベンチは **GitHub Actions（`.github/workflows/bench.yml`）だけ**が行う。
手元からのデプロイ、スモーク、ポータルの Enqueue はしない。並列に開発しても、どの変更でスコアがどう動いたかを 1 対 1 で追えるようにするため。

ベンチは 2 種類ある（run-name の末尾 ` replica` で見分ける。PR コメントの見出しと status check の名前にも出る）:

| 種類 | どこで | 何で測る | 誰が使う | 同時実行 |
|---|---|---|---|---|
| **公式**（`bench=official`） | 本番スタック `isucon-prod-2` | ポータルの公式ベンチ（運営の 1 台、チームで同時 1 本） | main の基準値、`workflow_dispatch`、`/bench --official` | 到着順に 1 本ずつ（`queue.sh`） |
| **自作**（`bench=replica`） | 複製スタック（本番と同じ CloudFormation テンプレートで作った別スタック。`ci/stacks/`） | `bench/cmd/isureal-bench`（公式の再現、`-manifest` 無しの load-only）を東京の EC2 から | PR の `/bench` | 複製スタックの数だけ並列（`stacklock.sh`） |

自作の点は本番の絶対値と一致しない（同じ commit を両方で測った比 = 較正を記録する。下の「較正」）。本番のサーバには PR のコードが乗らないので、公式ベンチの枠と本番の状態を PR の試行で消費しない。

## 使い方

PR は isucon26-container に作る。このリポジトリは monorepo なので、app のコード（`app/`）も設定（`infra/`）も同じブランチで直して 1 つの PR にする。

| やりたいこと | 方法 | どこに何がデプロイされるか |
|---|---|---|
| PR の変更を測る（push 後の測り直しも） | PR に `/bench [メモ]` とコメント（PR の open では走らない） | 空いている複製スタックに、その時点の PR の head（`app/` も `infra/` も）。自作ベンチ |
| PR を本番 + 公式ベンチで測る（最終候補の確認、較正） | PR に `/bench --official [メモ]` | 本番に PR の head。公式ベンチ（1 本ずつ） |
| main（基準値）を測る | `gh workflow run bench.yml -R Harineko0/isucon26-container`（または Actions の Run workflow ボタン）。マージしても自動では走らない | 本番に main の head。公式ベンチ |
| 任意の commit | `gh workflow run bench.yml -R Harineko0/isucon26-container -f container_ref=<ref>` | 本番に指定した commit。公式ベンチ |
| 較正（同じ commit を自作でも測る） | `gh workflow run bench.yml -R Harineko0/isucon26-container -f container_ref=<ref> -f bench=replica [-f stack=<name>]` | 複製スタックに指定した commit。自作ベンチ |

複製スタックが 1 つも無い（Variables の `BENCH_REPLICA_STACKS` が空）ときは、PR の `/bench` も従来どおり本番 + 公式ベンチで測り、コメントにその旨を出す。

- 3 台とも `app/webapp` を配る。`app-europe` `app-america` は `app` への symlink で、配らない。
- 結果は PR のコメント（キュー待ち → デプロイ中 → 実行中 → 結果、と 1 つのコメントが書き換わる）と、`bench-results` ブランチの `results.jsonl` に残る。
- ベンチした commit には commit status が付き、PR の status checks に出る（`/bench` での測り直しも、その時点の head に付く）。公式は `Benchmark`、自作は `Benchmark (isureal-bench)`。pending（キュー待ち・デプロイ中・実行中）→ success（`PASS 45500（前回比 +534 (+1.2%)）`）/ failure（ベンチ FAIL、17:30 以降の拒否）/ error（デプロイ等で失敗、またはベンチ中にサーバの状態が変わった）。Details から run に飛べる。
  `git fetch origin bench-results && git show FETCH_HEAD:results.jsonl | jq -c '{job_id,bench,stack,score,passed,label,app_sha,pair_ratio}'`
- スコアの比較は**同じ種類（公式どうし・自作どうし）**で行う。PR の run は**同じ PR の前回の run** と比べる（`前回比`）。その PR の初回だけ main の直近の PASS と比べる（`main 比 …、この PR の初回`。自作なら main を自作で測った直近の PASS）。main の run は main の直近の PASS と比べる。表には比較元の job と commit が並ぶ。公式の `job_id` はポータルのジョブ番号、自作の `job_id` は Actions の run_id。
- コメントにはほかに FAIL の理由、リージョン別の人数推移、ベンチャーの stdout が載る。
- PR をマージ・クローズすると、その PR の未完了の run（キュー待ち・実行中）は `.github/workflows/bench-cancel.yml` が自動でキャンセルする。実行中ならベンチの途中でも止まる（ポータルに投げ済みのジョブはポータル側で最後まで走り、次の run はその終了を待つ）。
- `BENCH_FREEZE_AT`（既定 2026-10-31 17:30 JST）以降は main 以外を拒否する（キュー待ちの前・後と、ポータルの空き待ち後のデプロイ直前に判定する。`ci/bench/freeze.sh`）。最終スコア = 最後のベンチなので、終盤に PR のコードがサーバに残らないようにするため。`BENCH_REPLICA_UNTIL`（競技日の朝に 16:15 JST を設定する。既定は 2099 年）以降は自作ベンチ（複製スタック）も拒否する（複製を消して再起動リハーサルに入るため）。

## 1 run の流れ（`ci/bench/`）

1. 順番待ち。公式: `queue.sh` が、自分より run_number が小さい未完了の**公式**の bench run が無くなるまで待つ（run-name が ` replica` で終わる自作の run は本番に触らないので数えない）。Actions の `concurrency` は待ちを 1 本しか持てず古い待ちを捨てるので使わない。自作: `stacklock.sh claim` が `BENCH_REPLICA_STACKS` から空いている複製スタックを取る（`bench-locks` ブランチの `<stack>.lock` を contents API で原子的に作る。全部使用中なら空くまで待つ。持ち主の run が completed なら古いロックとして消す）。取ったスタック名が `STACK`。
2. `run.sh`（`BENCH_KIND=official|replica`。`STACK` の IP は `ci/stacks/lib.sh` が `ci/stacks/<name>.env` から読み、`deploy.sh` / `infra/lib.sh` / `fingerprint.sh` / `measure.sh` も同じものを使う）
   1. 公式だけ: ポータルに PENDING/RUNNING のジョブがあれば終わるまで待つ。
   2. 公式だけ: CI の記録（results.jsonl）に無いポータルのジョブを探す。あれば誰かが手でベンチを投げたので警告に出す。
   3. `deploy.sh`（app を 3 台で並列にビルド・再起動し、各台で新しいプロセスが `:8080` を listen するまで最大 30 秒待つ）と `infra/deploy.sh`（設定を全台に配って reload、envcheck）。`deploy.sh` は 1 台でも rsync・build・restart・起動確認に失敗すると、失敗した台を `deploy.sh: FAILED on: <ip>` と出して非 0 で終わり、run はベンチせずに `failed:deploy-app` になる。複製スタックの初回は asia1 に PgBouncer のバイナリが無いので `infra/deploy.sh` が docker でビルドして配る（数分かかる）。
   4. ルーター経由で `POST /api/initialize` が 200 を返すか確かめる（壊れた版でベンチ枠を使わない）。所要時間を run のログ（`initialize: 14.4s`）と PR コメントに出し、`results.jsonl` に `init_sec` として残す。
   5. `fingerprint.sh` でサーバの状態（配った app の commit、バイナリの hash、isureal-go/nginx/PG の起動時刻）を記録し、app 3 台がこの run の app commit で動いているか確かめる。配った commit はサーバの `~/webapp/DEPLOYED_SHA` にある（中身はこのリポジトリの commit。サーバの `/home/isucon` は git 管理しない。今何が動いているかは `ssh isucon26-asia cat webapp/DEPLOYED_SHA`）。
   6. `measure.sh reset`（`pg_stat_statements_reset()` と 3 台の access.log の truncate）→ `measure.sh start`（4 台の `vmstat 1` と asia2 の wait event を 1 秒ごとに記録）→ ベンチ → `measure.sh stop`。公式は `portal.py enqueue` → `portal.py wait`。自作は `replica.sh`（`isureal-bench` を linux/amd64 の静的バイナリにビルドして `BENCH_RUNNER` の EC2 に scp し、複製スタックのルーター `http://<RouterPublicIP>:8080` に向けて走らせ、`bench.log` と `result.json` を持ち帰る）→ `replica_job.py`（ポータルのジョブと同じ形の `job.json` にする。以後は公式と同じ経路で記録・コメント）。
   7. もう一度 `fingerprint.sh` と `infra/deploy.sh -n` を取り、ベンチ中にデプロイ・再起動・設定変更が無かったか確かめる。あれば警告に出す。
   8. `measure.sh collect`: pg_stat_statements の上位と 3 台の alp（`alp.sh`）を取り、版（指紋）・CPU・DB wait・SQL・alp を PR コメントの「計測」に折りたたみで載せる。生データは run のログ（`===== pg-top.txt` など）。計測が失敗してもベンチの結果には影響しない（`measure.sh` は常に exit 0）。
3. `finalize.sh`（失敗・キャンセル時も実行）: 複製スタックのロックを返し（`stacklock.sh release`）、`bench-results` に記録し、コメントを最終版にする。ベンチが FAIL か run が失敗なら check を赤にする。

`portal.py` はポータルの Enqueue ボタンと同じリクエスト（protobuf、`X-CSRF-Token`）を送る。運営に「ポータル以外から類似リクエストでベンチを実行してよい」と確認済み。

単体テスト: `python3 ci/bench/report_test.py`（コメント・記録・status）、`bash ci/bench/stacklock_test.sh`（ロック。偽の `gh`）、`bash ci/bench/freeze_test.sh`（凍結と複製の期限）。後ろ 2 つは GNU の `date -d` / `timeout` を使うので Linux で（macOS なら coreutils）。

## 複製スタック（PR の自作ベンチ用）

複製スタックは、ポータルから落としたチーム固有のテンプレート（`contest_cloudformation.yaml`）で作る**本番と同じ構成の別スタック**（同じ AMI・初期データ・運営ルーター。マニュアル「複数スタックの問題」で複数作成は許されている）。プライベート IP（192.168.{1,2,3,4}.10）はテンプレートで固定なので、`infra/` の設定（nginx の map、pgbouncer.ini、env.sh）はそのまま使える。違うのは public IP だけで、それを `ci/stacks/<name>.env` に置く。

| 操作 | 誰が | コマンド |
|---|---|---|
| 作る | 人（手元） | `ALLOW_STACK_CHANGE=1 ci/stacks/create.sh isucon-replica-1` → `ci/stacks/isucon-replica-1.env` が書かれるので commit して main に入れる → `gh variable set BENCH_REPLICA_STACKS -R Harineko0/isucon26-container -b "isucon-replica-1 isucon-replica-2"`。4 台の EBS 初期化が終わるまで待つ |
| IP を調べ直す | 誰でも（読み取り） | `ci/stacks/resolve.sh [--write] <name>`（`describe-stacks` の Outputs から） |
| 消す | 人（手元） | `ALLOW_STACK_CHANGE=1 ci/stacks/delete.sh <name>` → `ci/stacks/<name>.env` を消して commit、`BENCH_REPLICA_STACKS` から外す |
| 残っていないか見る | 誰でも（読み取り） | `ci/stacks/check.sh`。CI でも `.github/workflows/replica-check.yml` が 30 分ごとに見て、残っていれば run を赤にし、`REPLICA_WARN_FROM`（既定 競技日 16:00）以降なら Slack と Issue `[P0][Ops]` に警告する（`AWS_ROLE_ARN` が要る） |

**危険**: 複製の 4 台も起動時にポータルへ自己登録するので、ポータルのサーバリストに本番と複製が混ざる。競技時間中は混ざっていてもよいが、**競技終了時点で混ざっていると失格**。手順は [docs/final-runbook.md](../docs/final-runbook.md) §0: **16:15 までに複製を全部消し、16:30 の再起動リハーサル（本番 4 台が再登録する）のあとサーバリストが本番 4 台の IP だけになっていることを確認する**。エージェントはスタックを作らない・消さない（hook が `cloudformation create-stack` / `delete-stack`、`ci/stacks/create.sh` / `delete.sh` を止める）。

複製スタックにも hook の保護は及ぶ（`.claude/hooks/guard-bench.py` は `ci/stacks/*.env` の IP を読む）。CI のベンチ中に手元から触ると、公式と同じく結果が汚れる。

## ベンチ実行元（`BENCH_RUNNER`）

自作ベンチは GitHub の runner からではなく、**東京リージョンの EC2**（Variables の `BENCH_RUNNER` = `ubuntu@<ip>`）で走らせる。GitHub の runner（米国）から叩くと往復 150 ms が乗って点が大きく下がり、較正にならない。EC2 は競技アカウントの `ap-northeast-1` に手で作る（c7i.large 程度、Ubuntu 24.04、競技の CloudFormation とは無関係のただの EC2。チームの GitHub の鍵 = `DEPLOY_SSH_KEY` でログインできること。Go は要らない: CI がビルドした静的バイナリを scp する）。複製スタックのルーターの 8080 はインターネットから叩けるので、EC2 を複製と同じ VPC に置く必要はない。複数の PR のベンチが同時に走ると同じ EC2 の CPU を分け合うので、複製スタックが 3 つ以上なら EC2 を大きくするか、`BENCH_RUNNER` を 1 台にせず別にする（1 変数なので、増やすなら `stacklock.sh` の次に runner も割り当てる改修が要る）。

`BENCH_RUNNER` が空なら GitHub の runner で走る（動作確認用）。`BENCH_FLAGS` で `isureal-bench` のフラグを足せる（`-days 5 -seed 1` など。`-manifest` は付けない: 本番の初期データの SecretKey が分からないので、利用者を登録して走る load-only。`-start-date` も付けない: 今開いている投稿期間の翌日から始め、初期データがもっと先の日付を持っていれば 409 を見て先の日付を探す）。

## 較正（公式と自作の比）

自作ベンチの絶対値は本番と一致しない（ベンチ側の挙動も、ベンチの実行元の性能も違う）。だから results.jsonl には `bench`（`official` / `isureal-bench`）と `stack` を残し、比較は同じ種類どうしで行う。加えて、**同じ commit を両方で測った組**があれば `pair_bench` / `pair_job_id` / `pair_score` / `pair_ratio`（公式/自作）を記録し、PR コメントに「較正: 同じ commit の公式 X、公式/自作 = r」と、直近 5 組の中央値から「公式換算の目安」を出す。較正を取るには main をマージしたあと `gh workflow run bench.yml -f bench=replica` も回す（公式の基準値は従来の `gh workflow run bench.yml`）。比が大きく動いたら、自作ベンチの前提（利用者の増え方、閲覧の配分）か複製スタックの状態（EBS 初期化、ウォームアップ）を疑う。自作ベンチだけの数字（`late_feed` / `stale_caption` / `stale_profile` = read-after-write の遅れ、`error_rate`、`load_sec`）も残る。

## 計測の読み方（#37）

- 統計（pg_stat_statements、access.log）のリセットはサーバの状態を変えるので、**CI が `run.sh` の中でベンチ直前にだけ行う**。手元から `pg_stat_statements_reset()` や truncate はしない（hook も止める）。だから CI のコメントの数字は、その run の版（コメント先頭の app の commit と「版」の指紋）のベンチ 1 本分だけ。
- 手元で追加に見るときは読み取りだけにする。alp: `tr -d '\r' < alp.sh | ssh isucon26-asia 'bash -s 30'`（列は count・ステータス別件数・apptime の合計/平均/最大。apptime 合計順で、SSE の `GET /api/notifications` は別枠）。SQL: `pgtop.sql` を asia2 で SELECT する。どちらも直近の CI run 以降の累積なので、別の run の数字が混ざっていないか DEPLOYED_SHA で確かめる。
- `*.sh` `*.py` `*.sql` は `.gitattributes` で LF に固定した。Windows で前から checkout している人は、`git add --renormalize .` は不要（index は LF）だが、作業ツリーの CRLF を直すには該当ファイルを消して `git checkout -- <file>` する。

## 索引の追加と initialize の時間（#40）

`init.sh`（= `POST /api/initialize`）は毎回 `002-indexes.sql`・`004`・`005`・`reset.sql` を流す。002 に `CREATE INDEX` を足すと、**最初の initialize の中で非 CONCURRENTLY に作られ、その間は表に書き込みロックがかかる**。

実測（2026-10-31、asia2 の PostgreSQL ログ `log_min_duration_statement` と pg_stat_statements、CI の run ログ）:

| 操作 | 時間 |
|---|---|
| `CREATE UNIQUE INDEX notifications_window_user ON notifications(posting_window_id, user_id)`（約 185 万行、heap 314 MB） | 5.3 秒 |
| `CREATE INDEX notifications_user_created ON notifications(user_id, created_at)` | 4.0 秒 |
| `CREATE INDEX realmojis_image_id`（約 120 万行）/ `posts_image_id`（約 63 万行） | 1.8 秒 / 1.7 秒 |
| `ALTER TABLE notifications DROP CONSTRAINT IF EXISTS …`（ACCESS EXCLUSIVE。ロック待ちがあると伸びる） | 最大 1.0 秒 |
| `reset.sql` の DELETE（notifications / realmojis / posts / comments、25 回の平均・最大） | 1.2・1.2・0.45・0.34 秒、最大 4.9 秒 |
| reset.sql 系の文の合計（1 回の initialize あたり平均） | 約 3.3 秒 |
| CI の initialize（デプロイ・再起動直後の 1 回目、ルーター経由。nginx の reqtime） | 14.4 秒（04:25 UTC の run） |
| 直後のベンチャー自身の initialize | 2.1 秒 |

ルール:

- **作るのに 1 秒を超えそうな索引（notifications・realmojis・posts・comments・image_objects の索引はすべて該当）は、initialize の中で作らない。** 先に人が asia2 で `CREATE INDEX CONCURRENTLY IF NOT EXISTS …` を流してから（CI のベンチ中でないとき、チームに宣言して）、002 には同じ名前で `CREATE INDEX IF NOT EXISTS …` を追記する。002 の文は既にある索引では一瞬で終わる。
- エージェントは DDL を流せない（hook が止める）。索引が要る PR は、PR 本文に「asia2 で先に流す SQL」を書いて人に頼む。
- 002 に `ALTER TABLE` / `DROP CONSTRAINT` を増やさない（毎回 ACCESS EXCLUSIVE を取る）。
- DB にだけあって sql/ に無い索引（`users_username_trgm`、`users_display_name_trgm` など）は、DB を作り直すと消える。002 に `IF NOT EXISTS` で書き戻す。
- ベンチャーの initialize の制限時間はマニュアルに書かれていない。CI は 60 秒で打ち切る。PR コメントの「CI の initialize」が前回より数秒以上伸びたら、002〜005 に重い文が入っていないか見る。

## 制約の仕組み

| 層 | 仕組み | 止めるもの |
|---|---|---|
| スクリプト | `ci/guard.sh`。`deploy.sh` / `smoke.sh` / `infra/deploy.sh`（`-n` 以外）は `GITHUB_ACTIONS` が無いと終了する | 手元からのデプロイ・スモーク |
| Claude Code | `.claude/settings.json` の PreToolUse hook（`.claude/hooks/guard-bench.py`） | 上記に加え、サーバへの rsync/scp、ssh での再起動・設定変更・ファイル書き換え、psql の書き込み・DDL、ルーターへの書き込み API、ポータルのベンチ一覧ページとベンチ投入 |
| CI | FIFO キュー、ポータルの空き待ち、ベンチ前後の指紋比較、記録外ジョブの検出 | 同時実行、汚染された結果を気づかずに信じること |

人がポータルを直接操作することは技術的には止められない。CI 側は「記録外ジョブ」「ベンチ中の状態変化」を検出してコメントで警告する。

読み取り（ssh での `journalctl` / `top` / `cat`、psql の `SELECT` / `EXPLAIN`、ルーターへの GET、`infra/deploy.sh -n`、`infra/pull.sh -n`）は自由にしてよい。

緊急時（CI が壊れた等）は、チームに宣言してから人が `ALLOW_LOCAL_DEPLOY=1 ./deploy.sh` を実行する。エージェントは使えない（hook が止める）。

## セットアップ（初回のみ）

Secrets:

| リポジトリ | 名前 | 中身 |
|---|---|---|
| isucon26-container | `PORTAL_COOKIE` | ブラウザでポータルを開き、DevTools → Network → 任意のリクエストの `Cookie` ヘッダの値をそのまま |
| isucon26-container | `DEPLOY_SSH_KEY` | 4 台に `isucon` で入れる秘密鍵（手元の `~/.ssh/id_ed25519_github` 相当） |
| isucon26-container | `DEPLOY_SSH_PASSPHRASE` | その鍵のパスフレーズ（パスフレーズなしの鍵なら不要）。CI は runner 上でだけパスフレーズを外して使う |

```bash
gh secret set PORTAL_COOKIE -R Harineko0/isucon26-container
gh secret set DEPLOY_SSH_KEY -R Harineko0/isucon26-container < ~/.ssh/id_ed25519_github
gh secret set DEPLOY_SSH_PASSPHRASE -R Harineko0/isucon26-container
```

| isucon26-container | `AWS_ROLE_ARN`（任意） | GitHub OIDC で assume できるロール（`cloudformation:DescribeStacks` / `ListStacks`、`ec2:DescribeInstances`）。`replica-check.yml` の消し忘れ検知と、`ci/stacks/<name>.env` が無い複製スタックの IP 解決に使う。無くても env ファイルを commit してあれば自作ベンチは動く |
| isucon26-container | `SLACK_WEBHOOK_URL`（任意） | 複製スタックの消し忘れを Slack に流す |

Variables（任意）: `BENCH_FREEZE_AT`（ISO 8601）、`PORTAL_TARGET_ID`（既定 1707 = asia1。ポータルのジョブ詳細の target と同じ）、`BENCH_REPLICA_STACKS`（複製スタック名、空白区切り。空なら PR も公式）、`BENCH_RUNNER`（自作ベンチの実行元 `user@host`）、`BENCH_FLAGS`、`BENCH_REPLICA_UNTIL`（複製を使う期限。競技日の朝に 16:15 JST を入れる）、`REPLICA_WARN_FROM`（消し忘れを Slack / Issue に出し始める時刻。既定 競技日 16:00）、`PROD_STACK`（既定 `isucon-prod-2`）。

ポータルの Cookie が切れると `portal.py` が `csrf: HTTP 302` などで落ちる。そのときは `PORTAL_COOKIE` を入れ直す。

## ベンチ ON/OFF（#272）

`bench-control` ブランチの `bench-control.json` が運用フラグ。
`{"enabled":true,"allowed_run_id":""}` で通常運転、false で新しいデプロイを待機させる。
OFF は実行中のデプロイ・ベンチを中断しない。OFF 後、既存 run とポータルの完了を確認してから再起動する。
待機は job の180分タイムアウトまで。取得失敗や不正JSONは安全側に失敗する。
`allowed_run_id` に指定した1本だけは OFF 中も通る。これは最終試験用で、他のrunが
サーバに触れていないことを確認してから設定する。指定runは停止中のFIFOを追い越す。
試験は同じ main SHA の通常時PASS → 4台をAWS CLIで再起動 → initialize前に投稿・画像を照合
→ 再起動後PASSの順。試験後は allowed_run_id を空に戻し、OFFを維持する。
導入前から走っているrunには適用されないため、導入時にすべて完了するまで待つ。
