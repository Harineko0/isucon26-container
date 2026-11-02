# isureal-bench: 公式ベンチマーカーの再現と、手元での並列ベンチ

ISUCON2026 (ISUReal) の公式ベンチマーカーは運営の 1 台だけで、チームで同時に 1 本しか回せなかった (約 2 分/本、93 本で 1 日が終わった)。
`bench/` は、公式ベンチの挙動を手元で再現し、**アプリ 3 台 + DB + ルーターのスタックを何組でも立てて同時に測る**ための道具。

| 道具 | 何をするか |
|---|---|
| `isureal-bench` | 公式ベンチの流れ (初期化 → 整合性チェック → 負荷 → 最終チェック) と採点を再現する負荷走行ツール |
| `isureal-datagen` | 初期データ (利用者・友達・投稿期間・投稿・画像・RealMoji・コメント・通知) を PostgreSQL に作り、ベンチが検証に使う `manifest.json` を書く |
| `isureal-router` | 競技のルーター (X-Country-Code で振り分け、X-Client-IP、遅延) の再現 |
| `local/` | docker compose で本番と同じ形 (nginx+app ×3 リージョン、共有 PostgreSQL、ルーター) を立て、1 コマンドでベンチする |

## 使い方

前提: Docker (compose v2)、Go 1.25 以上 (無ければ `--in-docker`)。

```bash
# 作業ツリーのアプリを small の初期データで測る (初回はイメージのビルドと datagen で数分かかる)
bench/local/run.sh

# 別の commit を測る (git archive した木をビルドする。作業ツリーは触らない)
bench/local/run.sh -r origin/main

# 2 つの版を同時に測る (スタック p1, p2 を別サブネット・別ポートで立てる)
bench/local/parallel.sh origin/main my-branch
bench/local/parallel.sh --scale tiny --bench "-days 3" . origin/main   # "." は作業ツリー

# ベンチのフラグは -- の後に
bench/local/run.sh -- -days 3 -log-level debug -seed 1

# 片付け (コンテナとボリューム)
bench/local/run.sh --down
bench/local/run.sh -n p1 -i 1 --down
```

結果は `bench/local/work/<name>/` に残る: `bench.log` (公式と同じ形のログ + 末尾の JSON)、`result.json` (配点別・エンドポイント別の内訳)、`manifest.json`。

ベンチだけを手で動かすなら:

```bash
cd bench && go build -o /tmp/isureal-bench ./cmd/isureal-bench
/tmp/isureal-bench -target http://127.0.0.1:28080 -manifest local/work/bench/manifest.json -out /tmp/result.json
```

`-manifest` を省くと初期データの検証を飛ばし、日 1 の利用者を `-initial-users` の人数だけ登録して走る (初期ユーザーの SecretKey が分からない環境向け)。

## 公式ベンチの何を再現しているか

根拠は [manual.md](../manual.md) と、CI の PR コメントに残った公式ベンチの stdout・alp (`docs/codex/08-benchmarks.md` の走行) 。内部実装は非公開なので、観測から組み立てた推定を含む (下記「分からないこと」)。

### 流れ (manual.md「負荷走行」)

1. `POST /api/initialize` (ルーター経由、JP)。200 でなければ FAIL。`language` が空なら FAIL。
2. 整合性チェック: 認証なしは 401、小文字の国コードは 400 (ルーター)、存在しないユーザーのログインは 401。manifest があれば初期ユーザーで ログイン (401 なら即 FAIL)・`GET /api/me`・Memories (日・月)・エクスポート (ZIP の posts.json と画像のハッシュ)・友達一覧を期待値と照合する。
3. 負荷走行用のユーザーの準備: manifest の seed 利用者 (asia 18 / europe 7 / america 6) でログイン。無ければ同数を登録して互いに友達にし、1 人を認定公開アカウントにする。
4. 負荷 (下記)。
5. 最終チェック: 削除した投稿の画像が 5 秒後に 403 になっていること。

### 負荷の時間割 (公式 stdout から)

```
T+0   asia 1 日目        T+3  europe 1 日目   T+6  america 1 日目
T+9   asia 2 日目        …   (同じリージョンは 9 秒ごと、リージョン間は 3 秒ずらし)
T+36  asia 5 日目        T+39 europe 5 日目   T+42 america 5 日目
T+51  最終日（グランプリ）: 3 リージョン同時に開く。約 8 秒で負荷終了
      → "グランプリ: アジア 924 人, ヨーロッパ 324 人, アメリカ 283 人" (最終日の検証通過数)
```

- 投稿期間は管理者 (`admin` / `password1234`) が `POST /api/admin/posting-window` で開く。localDate は manifest の最後の日の翌日から 1 日ずつ進む。`onTimeSeconds` は 5 (`-on-time-seconds`)。
- 利用者は投稿期間が開く 1 秒前に `GET /api/notifications` (SSE) を購読し、通知を受け取ってから動く (通知の受信 1 点)。3 秒届かなければ `GET /api/posting-window/current` で確かめて進む (エラーとして数える)。
- **人数の増え方**: 公式は asia 18 → 54 → 160 → 468 → 1155 (日 1→2 はちょうど 3 倍、以後は検証通過数に比例して鈍る)。ここでは「開いてから 5 秒時点の友達フィード検証通過数 × 2 人」を次の日までに登録する (`-growth`, `-max-users`)。登録した利用者は同リージョン 2 人・別リージョン 1 人に友達申請して承認され、1 件は拒否、1 件は取り下げ、認定公開アカウントを 1 人フォローする (公式 alp の登録 1 人あたり申請 5 件・承認 1.7 件に合わせた)。
- 数 ‰ の利用者が 1 日の終わりにリージョンを移動する (`PATCH /api/me {region}`、公式の "user moved")。移動前のリージョンのサーバに送ると 403 `region_mismatch` になることも見る。

### 利用者の 1 日

```
通知 → GET /api/posting-window/current (id が一致、canPost)
     → POST /api/posts (multipart、約 350 KB の JPEG、caption / visibility / location をランダム)
     → GET /api/posts/me/current (投稿詳細 1 点) → GET 画像 (2 点、内容を SHA-256 で照合)
     → [10% で] 署名を 1 文字変えた URL と期限切れの URL が 403
     → 友達フィードの検証: 今日すでに投稿している友達が GET /api/feeds/friends (3 ページまで) し、
        自分の投稿が載っていれば 10 点 (最終日は 20 点)。別リージョンの友達でも即反映 (read-after-write)
     → Memories で前回の投稿を確認 (GET /api/memories/<前回の localDate>、1 点)
     → [2% で] 投稿を削除 → 5.5 秒後に画像が 403 → 投稿し直す
     → 投稿期間が終わるまで閲覧・交流 (重み付きランダム、間隔 50〜150 ms):
        friends/fof/public/following フィード、画像、投稿詳細、RealMoji・コメント一覧、プロフィール、
        RealMoji 送信 (5 点)、コメント (3 点)、検索 (2 点)、友達申請→承認 (2+2 点)、一覧、フォロー/解除、
        Memories 月 (3 点)・日 (2 点)、過去の画像 (2 点)、エクスポート (20 点)、プロフィール変更 (1 点)、
        プロフィール画像 (3 点)、キャプション変更 (1 点)、ブロック→相手からは 404→解除、
        閲覧権限のない投稿 (友達でない人の friends 限定) が 404
     → 1 日の終わりに 1% 退会 (5 点、旧トークンと再ログインが 401)、0.4% 移動、30% ログアウト
```

重みは公式 alp の利用者・日あたりの件数 (friends フィード 1.3、画像 6.3、Memories 1.6、検索 1.0、友達申請 1.9、…) に寄せた。

### 採点 (manual.md「スコアの計算」)

配点表と倍率 (asia 1、europe / america 1.5) は `internal/score/score.go`。閲覧の上限 (利用者・投稿期間あたり: フィード 20、画像 4、投稿詳細 8、RealMoji・コメント一覧 8、プロフィール 4、RealMoji 送信 5、コメント 5) を守り、投稿直後の画像と投稿詳細は上限と別に数える。友達フィードの検証通過は利用者・投稿期間ごとに 1 回。合計は切り捨て。

### 検証と FAIL (manual.md「負荷走行の打ち切り」)

致命 (即 FAIL、スコア 0):

- 初期データのユーザーのログインが 401
- フィードに、自分の投稿・削除済みの投稿・終了した投稿期間の投稿・退会した利用者の投稿・ブロック関係の投稿・友達でない人の投稿 (friends)・visibility が合わない投稿が返る、並び順が違う
- 閲覧権限のない投稿が取得できる / その投稿への RealMoji・コメントが受理される / ブロックした相手からプロフィールが見える
- Memories・エクスポートが期待値 (manifest、またはベンチが投稿した内容) と一致しない
- 削除した投稿の画像が 5 秒を過ぎても取得できる、署名改竄・期限切れの URL で画像が取得できる
- 応答の構造や値が要求と一致しない (投稿の author / visibility / caption / postingWindowId、onTime と lateSeconds、登録・ログインの応答、…)

致命でないエラー (タイムアウト、5xx、期待外のステータス、3 秒以内に通知が来ない) は件数を数えて `messages` に残す。
書き込み直後の読み取りが古い値を返した回数は、致命にせず `result.json` の `lateFeedVisibility` (友達フィードに投稿が 1 回目で見えず再試行した)、`staleCaption` (キャプション変更後)、`staleProfile` (表示名・自己紹介の変更後) に数え、ログに WARN で値を出す。公式の打ち切り条件 (manual.md) に無い項目なので FAIL にはしないが、0 でなければ read-after-write の疑いとして見る。公式に減点は無いが、エラー率が `-max-error-rate` (既定 5%) を超えると FAIL にしている (公式の閾値は不明)。503 は 2 回まで再試行し、再試行してもエラーとして数える。

### 分からないこと・割り切り

- 公式の人数の増え方の式、閲覧行動の正確な配分、1 利用者の操作間隔、エラー率の閾値、初期化の制限時間、リクエストのタイムアウト (10 秒にした) は観測からの推定。
- 公式の初期データ (5.6k 人、投稿 63 万、画像 16 GB) そのものは手に入らない。`isureal-datagen` が同じ構造の小さいデータを作る (`--scale tiny / small / medium`、`-image-bytes` で画像の大きさ)。本番サーバの初期ユーザーの SecretKey は分からないので、本番に向けるときは `-manifest` 無し (初期データの検証を飛ばし、利用者を登録して走る) になる。
- 1 台のマシンで app ×3 + DB + ベンチが動くので、**絶対値は本番と比べられない**。同じマシンで同時に測った版同士の差を見る。並列に立てたスタックは CPU を分け合うので、本数を増やすほど 1 本あたりの点は下がる。手元 (Apple Silicon のノート、Docker Desktop) の実測: tiny の初期データ、同じアプリを 2 スタック同時に `-days 3` で測って 38,645 と 43,064 (約 10% の揺れ)。1 スタックで既定の 5 日 + 最終日では約 99,000 点、利用者は asia 75 → 535 人。
- 最終日 (グランプリ) は 3 リージョンが同時に開くので、手元では CPU が足りて europe / america の通過が 0 になることがある (投稿に数秒かかり、8 秒の最終日のうちに友達のフィードまで届かない)。本番 (c5.large / c7i.large が 4 台) とは条件が違う。`-grandprix-duration` を延ばすか `--scale tiny` で見る。
- リージョン間の遅延は europe / america のコンテナの egress に `tc netem delay 100ms` (本番の往復 100 ms 相当) を付けて再現する (`NETEM_DELAY=0` で無効、`NETEM_RATE=50mbit` で帯域も)。ルーター側の遅延は `ROUTER_LATENCY`。

## ローカル環境の形 (`local/`)

```
isureal-bench (ホスト) ──→ router (:28080 → NET_PREFIX.10.10)
                               ├─ JP/KR/CN/AU → asia-nginx + asia     (NET_PREFIX.1.10)
                               ├─ GB/DE/FR    → europe-nginx + europe (NET_PREFIX.2.10, netem 100ms)
                               └─ US/CA/MX    → america-nginx + america (NET_PREFIX.3.10, netem 100ms)
                                                       └─ db (PostgreSQL 16, NET_PREFIX.4.10)
```

- `NET_PREFIX=192.168` (index 0) なら IP が本番と同じになり、`infra/app/etc/nginx/sites-available/isureal.conf` の `map $remote_addr` (サーバ間の内部 API の認可) もそのまま働く。index N のスタックは `10.(200+N)` に置き換えて使う (`nginx-entrypoint.sh`)。
- アプリのコンテナは `ISUREAL_PEER_HOSTS` で相手リージョンの IP を知る (`image_store.go`)。nginx は本番と同じ設定、`/home/isucon/imgstore` と `imgcache` はアプリと nginx で共有する。
- PgBouncer (本番は asia1 の 6432) は立てていない。アプリは DB に直結する (`ISUREAL_APP_DB_POOL` を渡せば使える)。
- DB の初期データはスタックごとのボリュームに 1 回だけ作る。ベンチのたびに `POST /api/initialize` (= `init.sh` → `reset.sql`) で戻るのは本番と同じ。作り直すときは `--datagen`。

## 構成

```
bench/
  cmd/isureal-bench/      CLI
  cmd/isureal-datagen/    初期データ生成
  cmd/isureal-router/     ルーターの再現
  internal/api/           API の JSON 形 (manual.md と app/webapp/nodejs の参照実装から)
  internal/client/        HTTP クライアント (X-Country-Code / X-Client-IP、SSE、multipart、503 再試行、統計)
  internal/scenario/      利用者モデル、日ごとの投稿期間、各操作の検証と加点、成長、グランプリ
  internal/score/         配点表・倍率・上限
  internal/manifest/      初期データの期待値
  internal/jpegs/         ベンチが投稿する JPEG の生成
  local/                  docker compose、run.sh、parallel.sh
```
