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

const (
	// 接続の作り直しは EU/US で 1 本 ~0.43s (TCP + 起動 + SCRAM 認証の往復) かかるので寿命は長めにする。
	// ただしバックエンドのメモリ (プラン/ステートメントのキャッシュ) が際限なく育たないよう無期限にはしない。
	dbConnMaxLifetime = 30 * time.Minute
	// pgx stdlib は既定で「前回の取り出しから 1 秒超」の接続に、使う前に Ping (空クエリ = 1 往復) を打つ
	// (pgx v5.11.0 stdlib/sql.go の Conn.ResetSession)。EU/US ではこれが +100ms になるので、
	// 生存確認は長くアイドルだった接続 (DB 再起動後に死んでいる可能性がある) に限る。
	dbPingIdleThreshold = 30 * time.Second
)

// dbMaxConns はプールの上限。DB サーバ (メモリ 4GB, max_connections 200) は 3 台ぶんの
// バックエンドを抱えるので OOM にならない数に抑える。asia1 はリクエストが EU/US の約 5 倍で、
// 40 本ではベンチ中ずっと満杯になる (#68)。asia 80 + EU 40 + US 40 = 160。
func dbMaxConns(region string) int {
	if region == "asia" {
		return 80
	}
	return 40
}

// dbPlanCacheMode は アプリの接続だけに設定する plan_cache_mode (#153)。pgx は既定
// (QueryExecModeCacheStatement) で文を接続ごとに prepare するので、この設定が効く。
// custom plan (auto で汎用プランの見積もりが高いと毎回選ばれる) は計画のたびに参照する表の全索引をロックし、
// 頻出の文は 20〜38 リレーションになって fast-path の枠 (16) を超え LWLock:LockManager で待つ。
// generic plan は実行時に使う索引だけをロックし、計画の CPU も 1 回で済む。
// 起動パケットで送るので往復は増えない。psql など他の接続には影響しない。
const dbPlanCacheMode = "force_generic_plan"

func dbConnConfig(config *Config) (*pgx.ConnConfig, error) {
	connConfig, err := pgx.ParseConfig(config.DatabaseDSN())
	if err != nil {
		return nil, err
	}
	connConfig.RuntimeParams["plan_cache_mode"] = dbPlanCacheMode
	return connConfig, nil
}

func openDB(config *Config) (*sqlx.DB, error) {
	connConfig, err := dbConnConfig(config)
	if err != nil {
		return nil, err
	}
	sqlDB := stdlib.OpenDB(*connConfig, stdlib.OptionShouldPing(func(_ context.Context, p stdlib.ShouldPingParams) bool {
		return p.IdleDuration > dbPingIdleThreshold
	}))
	sqlDB.SetMaxOpenConns(dbMaxConns(config.Region))
	sqlDB.SetMaxIdleConns(dbMaxConns(config.Region))
	sqlDB.SetConnMaxLifetime(dbConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(0)
	return sqlx.NewDb(sqlDB, "pgx"), nil
}

// warmUpDBPool はプールの全接続を起動時に張っておき、ベンチ開始直後のリクエストが
// 接続確立の往復を払わないようにする。接続数は定常時 (ベンチ後) と同じなので DB の負荷は増えない。
func warmUpDBPool(db *sqlx.DB, n int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed int
	conns := make(chan interface{ Close() error }, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 新規接続は確立時点で認証まで済んでいるので Ping は不要。
			conn, err := db.Conn(ctx)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			conns <- conn
		}()
	}
	// 全部を同時に握ってから返さないと、同じ接続が使い回されて本数が増えない。
	wg.Wait()
	close(conns)
	for conn := range conns {
		_ = conn.Close()
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "db pool warm-up: %d/%d connections failed\n", failed, n)
	}
}
