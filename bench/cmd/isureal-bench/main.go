// isureal-bench は ISUCON2026 (ISUReal) の公式ベンチマーカーを再現する負荷走行ツール。
//
//	isureal-bench -target http://127.0.0.1:8080 -manifest manifest.json -out result.json
//
// stdout に公式と同じ形のログ (time=... level=INFO msg="asia 1 日目: 18 人") と、
// 末尾に {"pass","score","messages"} の JSON を出す。-out には内訳 (配点別・エンドポイント別) を書く。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"isureal-bench/internal/client"
	"isureal-bench/internal/manifest"
	"isureal-bench/internal/scenario"
)

func main() {
	cfg := scenario.DefaultConfig()
	var (
		target   = flag.String("target", "http://127.0.0.1:8080", "ルーターの URL (ベンチはここにしか話さない)")
		manPath  = flag.String("manifest", "", "isureal-datagen が書いた初期データの manifest.json (無ければ初期データの検証をせず、日 1 の利用者を登録する)")
		out      = flag.String("out", "", "詳細な結果 (JSON) を書くファイル")
		seed     = flag.Uint64("seed", uint64(time.Now().UnixNano()), "乱数の種")
		timeout  = flag.Duration("timeout", 10*time.Second, "1 リクエストのタイムアウト")
		initTO   = flag.Duration("initialize-timeout", 60*time.Second, "初期化のタイムアウト")
		skipInit = flag.Bool("skip-initialize", false, "POST /api/initialize を省く (開発用)")
		skipPre  = flag.Bool("skip-precheck", false, "整合性チェックを省く (開発用)")
		start    = flag.String("start-date", "", "manifest が無いときの最初の投稿期間の localDate (空なら自動: 今開いている投稿期間の翌日。409 なら先の日付を探す)")
		adminU   = flag.String("admin-user", "admin", "管理者のユーザー名")
		adminP   = flag.String("admin-password", "password1234", "管理者のパスワード")
		level    = flag.String("log-level", "info", "ログレベル (debug/info/warn/error)")
		initial  = flag.String("initial-users", "asia=18,europe=7,america=6", "manifest に Seed 利用者が無いときに日 1 で登録する人数")
		quiet    = flag.Bool("json-only", false, "ログを stderr に出し、stdout には結果の JSON だけを出す")
	)
	flag.IntVar(&cfg.Days, "days", cfg.Days, "通常の投稿期間の日数")
	flag.DurationVar(&cfg.WindowInterval, "window-interval", cfg.WindowInterval, "同じリージョンで次の投稿期間が開くまでの時間")
	flag.DurationVar(&cfg.Stagger, "stagger", cfg.Stagger, "リージョン間のずれ")
	flag.BoolVar(&cfg.GrandPrix, "grandprix", cfg.GrandPrix, "最終日 (全リージョン同時、20 点) を行う")
	flag.DurationVar(&cfg.GrandPrixDuration, "grandprix-duration", cfg.GrandPrixDuration, "最終日の長さ")
	flag.IntVar(&cfg.OnTimeSeconds, "on-time-seconds", cfg.OnTimeSeconds, "投稿期間の onTimeSeconds")
	flag.IntVar(&cfg.GrowthFactor, "growth", cfg.GrowthFactor, "検証通過 1 人あたり翌日に増える利用者数")
	flag.IntVar(&cfg.MaxUsersPerRegion, "max-users", cfg.MaxUsersPerRegion, "リージョンあたりの利用者の上限 (0 で無制限)")
	flag.DurationVar(&cfg.ThinkTime, "think", cfg.ThinkTime, "閲覧操作の間隔の目安")
	flag.IntVar(&cfg.PostImageBytes, "post-image-bytes", cfg.PostImageBytes, "投稿画像の大きさの目安")
	flag.IntVar(&cfg.RegisterParallel, "register-parallel", cfg.RegisterParallel, "登録の並列数")
	flag.Float64Var(&cfg.MaxErrorRate, "max-error-rate", cfg.MaxErrorRate, "この割合を超えるエラーで FAIL")
	flag.Parse()
	cfg.Seed = *seed

	for _, kv := range strings.Split(*initial, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		var n int
		fmt.Sscanf(v, "%d", &n)
		cfg.InitialUsers[k] = n
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*level)); err != nil {
		fmt.Fprintln(os.Stderr, "invalid -log-level:", *level)
		os.Exit(2)
	}
	logOut := os.Stdout
	if *quiet {
		logOut = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: lvl, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			a.Value = slog.StringValue(a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		return a
	}}))

	var man *manifest.Manifest
	if *manPath != "" {
		var err error
		man, err = manifest.Load(*manPath)
		if err != nil {
			log.Error("manifest を読めません", "err", err)
			os.Exit(2)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := client.New(*target, *timeout)
	world := scenario.New(cfg, c, log, man)
	log.Info("isureal-bench", "target", *target, "seed", cfg.Seed, "manifest", *manPath != "")
	res := world.Run(ctx, scenario.RunOptions{
		InitializeTimeout: *initTO, SkipInitialize: *skipInit, SkipPrecheck: *skipPre,
		StartDate: *start, AdminUsername: *adminU, AdminPassword: *adminP,
	})

	log.Info("結果", "pass", res.Pass, "score", res.Score, "requests", res.TotalRequests, "errors", res.ErrorCount, "users", res.Users, "posts", res.Posts)
	for _, l := range res.Breakdown {
		log.Debug("breakdown", "kind", l.Kind, "count", l.Count, "points", l.Points)
	}
	if *out != "" {
		data, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			log.Error("結果を書けません", "err", err)
		}
	}
	// 公式と同じ: 末尾に pass/score/messages の JSON
	summary := struct {
		Pass     bool     `json:"pass"`
		Score    int64    `json:"score"`
		Messages []string `json:"messages"`
	}{res.Pass, res.Score, res.Messages}
	if summary.Messages == nil {
		summary.Messages = []string{}
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Fprintln(os.Stdout, string(data))
	if !res.Pass {
		os.Exit(1)
	}
}
