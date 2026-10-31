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
// 40 本ではベンチ中ずっと満杯になる (#68)。asia 64 + EU 40 + US 40 = 144。
//
// asia を 80 → 64 にした理由 (OOM 対策): ベンチの山場で asia2 は MemAvailable 90MB、AnonPages 2.2GB、
// PageTables 287MB、Shmem 820MB、Committed_AS 4.0GB (CommitLimit 1.9GB) まで行き、バックエンド 1 本の
// 私有メモリは約 15〜18MB (プラン・カタログのキャッシュ、下の StatementCacheCapacity も参照)。
// 山場の asia2 はロードアベレージ 17〜26 (2 vCPU) で CPU が飽和しているので、同時に走る文を 80 → 64 に
// 絞っても DB 側のスループットは変わらず、待ちが asia1 のプールに移るだけ (asia1 → asia2 の往復は 1ms)。
// EU/US は往復 100ms で接続数がそのままスループット。europe1 はベンチ中ずっと満杯で、1 走行で接続待ちが
// 1 万回超・合計約 35 分あった (#154 の journal の dbstats)。asia から減らした 16 本を EU/US に 8 本ずつ回す
// (合計は 64 + 48 + 48 = 160 のまま。バックエンドの数は #187 の前と同じで、asia の文キャッシュ上限の分だけ軽い)。
func dbMaxConns(region string) int {
	if region == "asia" {
		return 64
	}
	return 48
}

// asia の接続ごとのサーバ側プリペアドステートメントの上限 (pgx の既定は 512 = 事実上無制限)。
// 1 ベンチで 190 種類の文が流れ、各接続は触った文の分だけ CachedPlanSource/Query/Plan (1 文あたり約 110KB) を
// 持ち続ける (pg_log_backend_memory_contexts: 13MB のうち約 7.5MB)。よく使う文は 1 ベンチで 60〜80 種類
// (pg_stat_statements で calls >= 10 が 77、>= 50 が 58) なので 64 なら熱い文はほぼ残り、月パーティションごとの
// 画像の文 (41 種類) など冷えた文だけが追い出される。溢れたときの代償は asia では 1ms の Prepare 往復と再プラン
// (1〜2ms)。EU/US は往復 100ms なので既定のまま。
const asiaStatementCacheCapacity = 64

func openDB(config *Config) (*sqlx.DB, error) {
	connConfig, err := pgx.ParseConfig(config.DatabaseDSN())
	if err != nil {
		return nil, err
	}
	if config.Region == "asia" {
		connConfig.StatementCacheCapacity = asiaStatementCacheCapacity
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
