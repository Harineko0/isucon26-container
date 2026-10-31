# CI ベンチ

デプロイとベンチは **GitHub Actions（`.github/workflows/bench.yml`）だけ**が、到着順に 1 本ずつ行う。
手元からのデプロイ、スモーク、ポータルの Enqueue はしない。並列に開発しても、どの変更でスコアがどう動いたかを 1 対 1 で追えるようにするため。

## 使い方

PR は isucon26-container に作る。このリポジトリは monorepo なので、app のコード（`app/`）も設定（`infra/`）も同じブランチで直して 1 つの PR にする。

| やりたいこと | 方法 | 何がデプロイされるか |
|---|---|---|
| PR の変更を測る | isucon26-container に PR を open する（自動で 1 回走る。draft は Ready for review にした時点） | PR の head（`app/` も `infra/` も） |
| push 後に測り直す | PR に `/bench [メモ]` とコメント | その時点の PR の head |
| main（基準値）を更新する | 自動: main に `app/**`・`infra/**`・`deploy.sh` を変える commit が入ったとき（PR のマージ）。文書や `ci/` だけの変更では走らない | main の head |
| main を手で測る | `gh workflow run bench.yml -R Harineko0/isucon26-container` | 同上 |
| 任意の commit | `gh workflow run bench.yml -R Harineko0/isucon26-container -f container_ref=<ref>` | 指定した commit |

- 3 台とも `app/webapp` を配る。`app-europe/` `app-america/` には各台の `env.sh` の写しがあるだけで、配らない。
- 結果は PR のコメント（キュー待ち → デプロイ中 → 実行中 → 結果、と 1 つのコメントが書き換わる）と、`bench-results` ブランチの `results.jsonl` に残る。
- ベンチした commit には commit status `Benchmark` が付き、PR の status checks に出る（`/bench` での測り直しも、その時点の head に付く）。pending（キュー待ち・デプロイ中・実行中）→ success（`PASS 45500（前回比 +534 (+1.2%)）`）/ failure（ベンチ FAIL、17:30 以降の拒否）/ error（デプロイ等で失敗、またはベンチ中にサーバの状態が変わった）。Details から run に飛べる。
  `git fetch origin bench-results && git show FETCH_HEAD:results.jsonl | jq -c '{job_id,score,passed,label,app_sha}'`
- スコアの比較: PR の run は**同じ PR の前回の run** と比べる（`前回比`）。その PR の初回だけ main の直近の PASS と比べる（`main 比 …、この PR の初回`）。main の run は main の直近の PASS と比べる。表には比較元の job と commit が並ぶ。
- コメントにはほかに FAIL の理由、リージョン別の人数推移、ベンチャーの stdout が載る。
- `BENCH_FREEZE_AT`（既定 2026-10-31 17:30 JST）以降は main 以外を拒否する。最終スコア = 最後のベンチなので、終盤に PR のコードがサーバに残らないようにするため。

## 1 run の流れ（`ci/bench/`）

1. `queue.sh`: 自分より run_number が小さい未完了の bench run が無くなるまで待つ。Actions の `concurrency` は待ちを 1 本しか持てず古い待ちを捨てるので使わない。
2. `run.sh`
   1. ポータルに PENDING/RUNNING のジョブがあれば終わるまで待つ。
   2. CI の記録（results.jsonl）に無いポータルのジョブを探す。あれば誰かが手でベンチを投げたので警告に出す。
   3. `deploy.sh`（app を 3 台で並列にビルド・再起動し、各台で新しいプロセスが `:8080` を listen するまで最大 30 秒待つ）と `infra/deploy.sh`（設定を全台に配って reload、envcheck）。`deploy.sh` は 1 台でも rsync・build・restart・起動確認に失敗すると、失敗した台を `deploy.sh: FAILED on: <ip>` と出して非 0 で終わり、run はベンチせずに `failed:deploy-app` になる。
   4. ルーター経由で `POST /api/initialize` が 200 を返すか確かめる（壊れた版でベンチ枠を使わない）。所要時間を run のログ（`initialize: 14.4s`）と PR コメントに出し、`results.jsonl` に `init_sec` として残す。
   5. `fingerprint.sh` でサーバの状態（配った app の commit、バイナリの hash、isureal-go/nginx/PG の起動時刻）を記録し、app 3 台がこの run の app commit で動いているか確かめる。配った commit はサーバの `~/webapp/DEPLOYED_SHA` にある（中身はこのリポジトリの commit。サーバの `/home/isucon` は git 管理しない。今何が動いているかは `ssh isucon26-asia cat webapp/DEPLOYED_SHA`）。
   6. `measure.sh reset`（`pg_stat_statements_reset()` と 3 台の access.log の truncate）→ `measure.sh start`（4 台の `vmstat 1` と asia2 の wait event を 1 秒ごとに記録）→ `portal.py enqueue` → `portal.py wait` → `measure.sh stop`。
   7. もう一度 `fingerprint.sh` と `infra/deploy.sh -n` を取り、ベンチ中にデプロイ・再起動・設定変更が無かったか確かめる。あれば警告に出す。
   8. `measure.sh collect`: pg_stat_statements の上位と 3 台の alp（`alp.sh`）を取り、版（指紋）・CPU・DB wait・SQL・alp を PR コメントの「計測」に折りたたみで載せる。生データは run のログ（`===== pg-top.txt` など）。計測が失敗してもベンチの結果には影響しない（`measure.sh` は常に exit 0）。
3. `finalize.sh`（失敗・キャンセル時も実行）: `bench-results` に記録し、コメントを最終版にする。ベンチが FAIL か run が失敗なら check を赤にする。

`portal.py` はポータルの Enqueue ボタンと同じリクエスト（protobuf、`X-CSRF-Token`）を送る。運営に「ポータル以外から類似リクエストでベンチを実行してよい」と確認済み。

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

Variables（任意）: `BENCH_FREEZE_AT`（ISO 8601）、`PORTAL_TARGET_ID`（既定 1707 = asia1。ポータルのジョブ詳細の target と同じ）。

ポータルの Cookie が切れると `portal.py` が `csrf: HTTP 302` などで落ちる。そのときは `PORTAL_COOKIE` を入れ直す。
