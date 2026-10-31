# 最後の 1.5 時間の手順書（#24）

最終スコア = **最後に回したベンチ**（FAIL なら記録なし）。終了後の追試では、4 台とルーターを再起動してからベンチを回し、**最終スコアの 75% を超える**こと、**ベンチで書いたデータが再起動後も読める**こと、envcheck とサーバリストが正しいことを見られる（manual.md「追試」）。

- この手順は**人が実行する**。エージェントは再起動・ポータル操作・ベンチ投入をしない（hook が止める）。エージェントにやらせてよいのは、下に「読み取り」と書いたコマンドの実行と結果の確認だけ。
- 役割を先に決める: **オペレーター 1 人**（AWS での再起動、`gh workflow` の操作、ポータルの確認）と、**確認係 1 人**（読み取りコマンドで結果を見る、記録を書く）。ほかの人はこの間 PR を open しない・`/bench` しない・マージしない。
- 定数: スタック `isucon-prod-2`、`P='--profile AdministratorAccess-573111099202 --region ap-northeast-1'`、ルーター `http://35.73.68.236:8080`。
- インスタンス ID（2026-10-31 に確認）: App1 = asia1 `i-0f626866418d74974`、App2 = europe1 `i-0cb8d3965b54de263`、App3 = america1 `i-048c33eadb020d481`、Database = asia2 `i-0079df54c53c2ae0b`。Router `i-0fbc3e17d2804b6a1` は**再起動しない**（ログイン・設定変更禁止。追試では運営が再起動する）。
  念のため再確認: `aws cloudformation describe-stack-resources --stack-name isucon-prod-2 $P --query "StackResources[?ResourceType=='AWS::EC2::Instance'].[LogicalResourceId,PhysicalResourceId]" --output text`

## タイムライン

| 時刻 | やること |
|---|---|
| 16:15 | 機能凍結。最終候補を main にマージし終える。main の自動ベンチが PASS したことを確認する |
| 16:30–17:00 | 再起動リハーサル（下の §1） |
| 17:00 | リーダーボード凍結（手順に影響なし） |
| 17:00–17:30 | リハーサルで見つかった問題の修正だけ。修正したら main で再ベンチ |
| 17:30– | CI は main 以外を拒否する。§2 の最終ベンチ |
| 17:50 | 最後のベンチ投入の締め切り（CI の 1 run はデプロイ込みで約 4 分） |
| 17:55 | §3 の終了時チェック |

## §1 再起動リハーサル（16:30 頃、約 30 分）

### 1-1. CI を止める（オペレーター）

```bash
# 走っている・待っている bench run が無いこと（あれば終わるまで待つ）
gh run list -R Harineko0/isucon26-container -w bench --status in_progress
gh run list -R Harineko0/isucon26-container -w bench --status queued
# リハーサル中に新しい run が始まらないようにする（§1-5 で戻す）
gh workflow disable bench.yml -R Harineko0/isucon26-container
```

ポータルのベンチ一覧（<https://portal.isucon.net/contestant/benchmark_jobs>）にも WAITING / RUNNING が無いことを目で見る。

### 1-2. 再起動前の記録（確認係、読み取り）

```bash
# 各台で動いている版（3 台とも main の commit であること）
for h in isucon26-asia isucon26-europe isucon26-america; do ssh $h 'hostname; cat ~/webapp/DEPLOYED_SHA'; done
# データ量（再起動後に同じか増えていること）。ベンチで書いた行が入っている
ssh isucon@3.115.193.33 "sudo -u postgres psql -d isureal -AtX -c \"select (select count(*) from users) users, (select count(*) from posts) posts, (select max(created_at) from posts) last_post, (select count(*) from comments) comments, (select count(*) from realmojis) realmojis, (select count(*) from notifications) notifications\"" | tee before.txt
# 直近の main のスコア（温まった状態の基準）
git fetch -q origin bench-results && git show FETCH_HEAD:results.jsonl | jq -c 'select(.label=="main") | {job_id,score,passed,app_sha,init_sec}' | tail -3
```

### 1-3. 4 台を再起動する（オペレーター）

```bash
aws ec2 reboot-instances $P --instance-ids i-0f626866418d74974 i-0cb8d3965b54de263 i-048c33eadb020d481 i-0079df54c53c2ae0b
aws ec2 wait instance-status-ok $P --instance-ids i-0f626866418d74974 i-0cb8d3965b54de263 i-048c33eadb020d481 i-0079df54c53c2ae0b
```

`reboot-instances` を使う（stop/start ではない。IP は変わらない）。終わるまで数分。

### 1-4. 再起動後の確認（確認係、読み取り）

```bash
# 起動直後であること、サービスが enabled かつ active であること
for h in isucon26-asia isucon26-europe isucon26-america; do
  ssh $h 'hostname; uptime; for s in isureal-go nginx isucon-register; do echo "$s $(systemctl is-enabled $s) $(systemctl is-active $s)"; done; cat ~/webapp/DEPLOYED_SHA'
done
ssh isucon@3.115.193.33 'hostname; uptime; for s in postgresql@16-main isucon-register; do echo "$s $(systemctl is-enabled $s) $(systemctl is-active $s)"; done'
# envcheck が 4 台とも exit 0（「全ての追試環境チェックをパスしました」）
for h in isucon26-asia isucon26-europe isucon26-america isucon@3.115.193.33; do ssh $h 'sudo /usr/local/bin/envcheck 2>&1 | grep -m1 message; echo "$(hostname) envcheck exit=${PIPESTATUS[0]}"'; done
# データが残っていること（before.txt と同じ）
ssh isucon@3.115.193.33 "sudo -u postgres psql -d isureal -AtX -c \"select (select count(*) from users) users, (select count(*) from posts) posts, (select max(created_at) from posts) last_post, (select count(*) from comments) comments, (select count(*) from realmojis) realmojis, (select count(*) from notifications) notifications\"" | diff before.txt - && echo "DATA OK"
# ルーター経由でフロントが返ること
curl -s -o /dev/null -w '%{http_code}\n' http://35.73.68.236:8080/
```

期待する結果: isureal-go / nginx / postgresql@16-main / isucon-register がすべて `enabled active`、DEPLOYED_SHA が 1-2 と同じ、envcheck が 4 台とも `exit=0`、`DATA OK`、`200`。

`isureal-go` は asia2 より先に起動して DB に繋がらず落ちることがあるが、`Restart=on-failure`（5 秒ごと）で戻るはず。`activating` / `failed` のままなら `journalctl -u isureal-go -b` を見る。

さらにブラウザ（Chrome）で <http://35.73.68.236:8080> を開き、manual.md「アプリケーションの表示確認」のユーザーでログインして、フィードと**画像**が表示されることを見る（画像をローカルにキャッシュしている場合、再起動後も返せることの確認）。

### 1-5. 冷えた状態でベンチ（オペレーター）

```bash
gh workflow enable bench.yml -R Harineko0/isucon26-container
gh workflow run bench.yml -R Harineko0/isucon26-container   # main を測る（note は空でよい）
```

CI は app を配り直して isureal-go を再起動するが、PostgreSQL は再起動しないので、DB のキャッシュは冷えたまま測れる（追試の初回に近い）。結果の PR コメントは無い（main の run）ので、run の Summary か `results.jsonl` で見る。

### 1-6. 判定と記録（確認係）

- 冷えたスコア / 温まった main のスコア を出す。**0.85 以上なら OK**（追試の 75% に余裕を持たせる）。下回ったら、冷えた状態で遅い所（PR コメントの「計測」相当を run のログで見る）を直すか、最終ベンチを温まった状態で回しすぎない（最終スコアが高いほど 75% の線も上がる）ことを考える。
- 記録: issue #24 に「時刻、冷えたスコア、温まったスコア、比、envcheck 4 台の結果、DATA OK」をコメントする（人が書く）。

### 失敗したとき

| 症状 | 見る所 |
|---|---|
| サービスが inactive | `systemctl is-enabled`。`disabled` なら `infra/` の unit / `WantedBy` を直して PR → main |
| データが減った | 耐久性を下げる設定が入っていないか（`infra/db/.../conf.d/`、`synchronous_commit`、`fsync`）。メモリにしか無いデータ（アプリ内キャッシュに書いて DB に書いていない）が無いか |
| envcheck が失敗 | 出力の項目を直す。`isucon-register.service`・envcheck 関連ファイル・セキュリティグループに触っていないか |
| 冷えたスコアが低い | pg_prewarm（#35）が効いているか、起動時のキャッシュ構築が initialize に間に合っているか |

## §2 最終ベンチ（17:30 以降）

- CI が main 以外を拒否する（`BENCH_FREEZE_AT`）。**main へのマージは 17:30 までに終える**。17:30 以降のマージは、リハーサル済みの版を壊す修正以外しない。
- 回し方: `gh workflow run bench.yml -R Harineko0/isucon26-container`（オペレーターだけ。ほかの人は回さない）。§1-1 で disable したままなら `gh workflow enable bench.yml -R Harineko0/isucon26-container` を先に。
- **最後のベンチは必ず PASS させる**: 17:30 以降は、すでに PASS したことのある main の commit だけを回す。PASS で十分なスコアが出たら、**それ以上回さない**（次の 1 本が FAIL すると記録が無くなる）。
- 回した run が FAIL / error になったら、17:50 までならすぐもう一度 main を回す。17:50 を過ぎたら回さない（終了までに終わらない可能性がある）。そのときの最後のベンチが PASS であることを、ポータルのベンチ一覧で確かめる。
- ベンチ中（WAITING / RUNNING）は誰も deploy・再起動・DB 書き込みをしない（CI 以外からはもともと禁止）。
- ベンチ中と最終ベンチの前は、app 3 台で imgcache を走査するコマンド（`du` / `find` / `ls -R` / imgsync）を打たない。inode cache が膨らみ、メモリ回収で sys が上がる（#240）。

## §3 終了時チェック（17:55 頃、オペレーター + 確認係）

```bash
# 4 台とルーターが running（停止したまま終わると追試できず失格）
aws ec2 describe-instances $P --instance-ids i-0f626866418d74974 i-0cb8d3965b54de263 i-048c33eadb020d481 i-0079df54c53c2ae0b i-0fbc3e17d2804b6a1 \
  --query 'Reservations[].Instances[].[InstanceId,State.Name,PublicIpAddress]' --output text
# 別のスタックが無いこと（isucon-prod-2 だけ）
aws cloudformation list-stacks $P --stack-status-filter CREATE_COMPLETE UPDATE_COMPLETE UPDATE_ROLLBACK_COMPLETE --query 'StackSummaries[].StackName' --output text
# envcheck（§1-4 のループをもう一度）
```

- ポータルのサーバリスト（<https://portal.isucon.net/contestant/contestant_instances>）に、**この 4 台の IP（13.159.52.104 / 52.199.206.161 / 52.197.40.86 / 3.115.193.33）だけ**が出ていること（別スタックのサーバが混ざったまま終わると失格）。
- 最後のベンチが PASS で、その `/api/initialize` の `language` が `go` であること（CI の run ログに出る）。
- 18:00 以降は、運営のアナウンスまでサーバに接続しない・操作しない。
