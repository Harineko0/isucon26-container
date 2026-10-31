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

func openDB(config *Config) (*sqlx.DB, error) {
	connConfig, err := pgx.ParseConfig(config.DatabaseDSN())
	if err != nil {
		return nil, err
	}
	options := []stdlib.OptionOpenDB{stdlib.OptionShouldPing(func(_ context.Context, p stdlib.ShouldPingParams) bool {
		return p.IdleDuration > dbPingIdleThreshold
	})}
	if preparedWarmEnabled(config.Region) {
		// 前回のプロセスでよく実行した文面を、接続を張った直後に 1 往復で Prepare しておく (prepared_warm.go)。
		if schema, err := schemaFingerprint(preparedWarmSchemaDir); err != nil {
			fmt.Fprintf(os.Stderr, "prepared-warm: disabled: %v\n", err)
		} else {
			setWarmStatements(loadWarmStatementsFile(preparedWarmFile, schema))
			connConfig.Tracer = executedStatements
			options = append(options, stdlib.OptionAfterConnect(afterConnectWarm))
			go saveHotStatementsLoop(preparedWarmFile, schema)
		}
	}
	sqlDB := stdlib.OpenDB(*connConfig, options...)
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
