package isureal

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestRequestPoolKeepsSessionConnectionsOnPrimary(t *testing.T) {
	for region, want := range map[string]int{"asia": 64, "europe": 128, "america": 128} {
		t.Run(region, func(t *testing.T) {
			cfg := &Config{Region: region, DBHost: "127.0.0.1", DBPort: "5432", DBSSLMode: "disable"}
			primary, err := openDB(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer primary.Close()
			direct, control, err := openRequestDBs(cfg, primary, "")
			if err != nil || direct != primary || control != primary || direct.Stats().MaxOpenConnections != dbMaxConns(region) {
				t.Fatalf("direct pool changed: %v", err)
			}
			pooled, control, err := openRequestDBs(cfg, primary, "127.0.0.2:6432")
			if err != nil {
				t.Fatal(err)
			}
			defer pooled.Close()
			defer control.Close()
			if pooled == primary || control == primary || control == pooled || pooled.Stats().MaxOpenConnections != want || primary.Stats().MaxOpenConnections != 2 || control.Stats().MaxOpenConnections != 2 {
				t.Fatalf("unexpected limits: app %d, watcher %d, control %d", pooled.Stats().MaxOpenConnections, primary.Stats().MaxOpenConnections, control.Stats().MaxOpenConnections)
			}
			if cfg.DBHost != "127.0.0.1" || cfg.DBPort != "5432" {
				t.Fatal("direct connection settings were overwritten")
			}
		})
	}
}

// PostgreSQL に直接つなぐ接続は plan_cache_mode を起動パケットで送り、文を prepare する実行モード
// (generic plan が効くモード) のままであること。PgBouncer 宛てには送らないこと (PgBouncer は知らない起動
// パラメータを拒み、接続できなくなる。PgBouncer 側は pgbouncer.ini の connect_query で設定する) (#153)。
func TestDBConnConfigPlanCacheMode(t *testing.T) {
	config := &Config{DBHost: "192.0.2.1", DBPort: "5432", DBUser: "isureal", DBPassword: "isureal", DBName: "isureal", DBSSLMode: "disable"}
	direct, err := dbConnConfig(config, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := direct.RuntimeParams["plan_cache_mode"]; got != "force_generic_plan" {
		t.Errorf("direct: plan_cache_mode = %q, want force_generic_plan", got)
	}
	if got := direct.DefaultQueryExecMode; got != pgx.QueryExecModeCacheStatement {
		t.Errorf("DefaultQueryExecMode = %v, want QueryExecModeCacheStatement", got)
	}
	pooled, err := dbConnConfig(config, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := pooled.RuntimeParams["plan_cache_mode"]; ok {
		t.Errorf("PgBouncer connection sends plan_cache_mode = %q in the startup packet", got)
	}
}

// pgbouncer.ini が PgBouncer のサーバー接続に plan_cache_mode を設定すること (#153)。
func TestPgBouncerSetsPlanCacheMode(t *testing.T) {
	ini, err := os.ReadFile("../../../infra/hosts/asia1/etc/isureal/pgbouncer.ini")
	if err != nil {
		t.Fatal(err)
	}
	want := "connect_query='SET plan_cache_mode = " + dbPlanCacheMode + "'"
	if !strings.Contains(string(ini), want) {
		t.Errorf("pgbouncer.ini does not contain %s", want)
	}
	if strings.Contains(string(ini), "ignore_startup_parameters") {
		t.Errorf("pgbouncer.ini ignores startup parameters; plan_cache_mode would be dropped silently")
	}
}
