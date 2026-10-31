package isureal

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestDBConnConfigPlanCacheMode は アプリの接続が起動パケットで plan_cache_mode を送り、
// 文を prepare する実行モード (generic plan が効くモード) のままであることを確かめる (#153)。
func TestDBConnConfigPlanCacheMode(t *testing.T) {
	config := &Config{DBHost: "192.0.2.1", DBPort: "5432", DBUser: "isureal", DBPassword: "isureal", DBName: "isureal", DBSSLMode: "disable"}
	connConfig, err := dbConnConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if got := connConfig.RuntimeParams["plan_cache_mode"]; got != "force_generic_plan" {
		t.Errorf("plan_cache_mode = %q, want force_generic_plan", got)
	}
	if got := connConfig.DefaultQueryExecMode; got != pgx.QueryExecModeCacheStatement {
		t.Errorf("DefaultQueryExecMode = %v, want QueryExecModeCacheStatement", got)
	}
	if connConfig.Host != "192.0.2.1" || connConfig.Database != "isureal" {
		t.Errorf("DSN fields lost: host=%q db=%q", connConfig.Host, connConfig.Database)
	}
}
