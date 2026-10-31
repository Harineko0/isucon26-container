package isureal

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// TestPgBouncerPlanCacheModeOnPostgres は本物の PgBouncer (本番と同じ 1.26.1、pool_mode = transaction、
// pgbouncer.ini の [databases] の行) を通して、アプリの接続で plan_cache_mode が force_generic_plan になり、
// PgBouncer 宛てに起動パケットで送ると接続できないことを確かめる (#153)。
// ISUREAL_TEST_BOUNCER_HOST / _PORT が無ければ飛ばす (sql/tests/pgbouncer_plan_cache.sh)。
func TestPgBouncerPlanCacheModeOnPostgres(t *testing.T) {
	host, port := os.Getenv("ISUREAL_TEST_BOUNCER_HOST"), os.Getenv("ISUREAL_TEST_BOUNCER_PORT")
	if host == "" || port == "" {
		t.Skip("ISUREAL_TEST_BOUNCER_HOST/PORT are not set (see app/webapp/sql/tests/pgbouncer_plan_cache.sh)")
	}
	ctx := context.Background()
	config := &Config{Region: "europe", DBHost: host, DBPort: port, DBUser: "isureal", DBPassword: "isureal", DBName: "isureal", DBSSLMode: "disable"}

	// アプリと同じ経路 (openRequestDB → PgBouncer 宛て) で開いた接続。複数の接続・prepare した文でも設定が見える。
	primary, err := openDB(&Config{Region: "europe", DBHost: "192.0.2.1", DBPort: "5432", DBSSLMode: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	db, control, err := openRequestDBs(config, primary, host+":"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer control.Close()
	conns := make([]*sqlx.Conn, 4)
	for i := range conns {
		if conns[i], err = db.Connx(ctx); err != nil {
			t.Fatalf("connect through PgBouncer: %v", err)
		}
		var mode string
		if err := conns[i].GetContext(ctx, &mode, `SELECT current_setting('plan_cache_mode')`); err != nil {
			t.Fatal(err)
		}
		if mode != dbPlanCacheMode {
			t.Errorf("conn %d: plan_cache_mode = %q through PgBouncer, want %q", i, mode, dbPlanCacheMode)
		}
	}
	for _, c := range conns {
		c.Close()
	}

	// 起動パケットで送ると PgBouncer が拒む (#228 の最初の版はこれで全リクエストが失敗していた)。
	direct, err := dbConnConfig(config, true)
	if err != nil {
		t.Fatal(err)
	}
	rejected := sqlx.NewDb(stdlib.OpenDB(*direct), "pgx")
	defer rejected.Close()
	if err := rejected.PingContext(ctx); err == nil {
		t.Error("PgBouncer accepted plan_cache_mode in the startup packet; the connect_query workaround may be unnecessary")
	} else {
		t.Logf("startup packet with plan_cache_mode through PgBouncer: %v", err)
	}
}
