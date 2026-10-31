# CI ベンチ

デプロイとベンチは **GitHub Actions（`.github/workflows/bench.yml`）だけ**が、到着順に 1 本ずつ行う。
手元からのデプロイ、スモーク、ポータルの Enqueue はしない。並列に開発しても、どの変更でスコアがどう動いたかを 1 対 1 で追えるようにするため。

## 使い方

PR は isucon26-container に作る。app のコード変更は、`app/` サブモジュールの中でブランチを切ってコミット・push し、container 側のブランチでサブモジュールの指す commit を進めて PR にする。

| やりたいこと | 方法 | 何がデプロイされるか |
|---|---|---|
| PR の変更を測る | isucon26-container の PR に `/bench [メモ]` とコメント | PR の head（infra/ 等）と、その commit の `app/` が指す commit |
| main（基準値）を測る | `gh workflow run bench.yml -R Harineko0/isucon26-container` | main と、main の `app/` が指す commit |
| 任意の組み合わせ | `gh workflow run bench.yml -R Harineko0/isucon26-container -f container_ref=<ref> [-f app_ref=<sha>]` | 指定どおり（`app_ref` を省くとサブモジュールの指す commit） |

- `app/` の commit は isucon26 に push 済みである必要がある（CI はそこから取る）。`app-europe/` `app-america/` は使わない（3 台とも `app/` を配る）。
- 結果は PR のコメント（キュー待ち → デプロイ中 → 実行中 → 結果、と 1 つのコメントが書き換わる）と、`bench-results` ブランチの `results.jsonl` に残る。
  `git fetch origin bench-results && git show FETCH_HEAD:results.jsonl | jq -c '{job_id,score,passed,label,app_sha}'`
- コメントには「main の直近の PASS」と「直前の CI ベンチ」との比較、FAIL の理由、リージョン別の人数推移、ベンチャーの stdout が載る。
- `BENCH_FREEZE_AT`（既定 2026-10-31 17:30 JST）以降は main 以外を拒否する。最終スコア = 最後のベンチなので、終盤に PR のコードがサーバに残らないようにするため。

## 1 run の流れ（`ci/bench/`）

1. `queue.sh`: 自分より run_number が小さい未完了の bench run が無くなるまで待つ。Actions の `concurrency` は待ちを 1 本しか持てず古い待ちを捨てるので使わない。
2. `run.sh`
   1. ポータルに PENDING/RUNNING のジョブがあれば終わるまで待つ。
   2. CI の記録（results.jsonl）に無いポータルのジョブを探す。あれば誰かが手でベンチを投げたので警告に出す。
   3. `deploy.sh`（app を 3 台でビルド・再起動）と `infra/deploy.sh`（設定を全台に配って reload、envcheck）。
   4. ルーター経由で `POST /api/initialize` が 200 を返すか確かめる（壊れた版でベンチ枠を使わない）。
   5. `fingerprint.sh` でサーバの状態（配った app の commit、バイナリの hash、isureal-go/nginx/PG の起動時刻）を記録し、app 3 台がこの run の app commit で動いているか確かめる。配った commit はサーバの `~/webapp/DEPLOYED_SHA` にある（rsync で配るのでサーバの `git log` は進まない。今何が動いているかは `ssh isucon26-asia cat webapp/DEPLOYED_SHA`）。
   6. `portal.py enqueue` → `portal.py wait`。
   7. もう一度 `fingerprint.sh` と `infra/deploy.sh -n` を取り、ベンチ中にデプロイ・再起動・設定変更が無かったか確かめる。あれば警告に出す。
3. `finalize.sh`（失敗・キャンセル時も実行）: `bench-results` に記録し、コメントを最終版にする。ベンチが FAIL か run が失敗なら check を赤にする。

`portal.py` はポータルの Enqueue ボタンと同じリクエスト（protobuf、`X-CSRF-Token`）を送る。運営に「ポータル以外から類似リクエストでベンチを実行してよい」と確認済み。

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
| isucon26-container | `BENCH_GH_TOKEN` | private な isucon26（app サブモジュール）を読める PAT（Contents read） |

```bash
gh secret set PORTAL_COOKIE -R Harineko0/isucon26-container
gh secret set DEPLOY_SSH_KEY -R Harineko0/isucon26-container < ~/.ssh/id_ed25519_github
gh secret set DEPLOY_SSH_PASSPHRASE -R Harineko0/isucon26-container
gh secret set BENCH_GH_TOKEN -R Harineko0/isucon26-container
```

Variables（任意）: `BENCH_FREEZE_AT`（ISO 8601）、`PORTAL_TARGET_ID`（既定 1707 = asia1。ポータルのジョブ詳細の target と同じ）。

ポータルの Cookie が切れると `portal.py` が `csrf: HTTP 302` などで落ちる。そのときは `PORTAL_COOKIE` を入れ直す。
