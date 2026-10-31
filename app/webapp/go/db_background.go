package isureal

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// 専用プール (#44)。投稿期間の監視 (20ms ごと)、IP BAN の再読込 (200ms ごと)、購読者の確認 (1s ごと)、
// 管理 API (投稿期間の開始) は、リクエスト用のプール (asia1 ではベンチ中ずっと 40/40 で、取り出し待ちが
// 平均 0.4〜0.7s) に並ばせず、ここから接続を取る。投稿期間の開始を検知するまでの遅れが、そのまま利用者の
// 投稿の遅れになるため。本数は同時に使う 4 者ぶん。DB の接続数は 1 台あたりこの分だけ増える。
const dbBackgroundMaxConns = 4

func openBackgroundDB(config *Config) (*sqlx.DB, error) {
	connConfig, err := pgx.ParseConfig(config.DatabaseDSN())
	if err != nil {
		return nil, err
	}
	sqlDB := stdlib.OpenDB(*connConfig, stdlib.OptionShouldPing(func(_ context.Context, p stdlib.ShouldPingParams) bool {
		return p.IdleDuration > dbPingIdleThreshold
	}))
	sqlDB.SetMaxOpenConns(dbBackgroundMaxConns)
	sqlDB.SetMaxIdleConns(dbBackgroundMaxConns)
	sqlDB.SetConnMaxLifetime(dbConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(0)
	return sqlx.NewDb(sqlDB, "pgx"), nil
}

// warmUpBackgroundDB は専用プールの接続を起動時に全部張っておく (warmUpDBPool と同じ理由)。
func warmUpBackgroundDB(db *sqlx.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	conns := make(chan interface{ Close() error }, dbBackgroundMaxConns)
	for range dbBackgroundMaxConns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "background db pool warm-up: %v\n", err)
				return
			}
			conns <- conn
		}()
	}
	wg.Wait()
	close(conns)
	for conn := range conns {
		_ = conn.Close()
	}
}
