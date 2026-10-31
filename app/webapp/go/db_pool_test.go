package isureal

import "testing"

func TestRequestPoolKeepsSessionConnectionsOnPrimary(t *testing.T) {
	for region, want := range map[string]int{"asia": 64, "europe": 128, "america": 128} {
		t.Run(region, func(t *testing.T) {
			cfg := &Config{Region: region, DBHost: "127.0.0.1", DBPort: "5432", DBSSLMode: "disable"}
			primary, err := openDB(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer primary.Close()
			direct, err := openRequestDB(cfg, primary, "")
			if err != nil || direct != primary || direct.Stats().MaxOpenConnections != dbMaxConns(region) {
				t.Fatalf("direct pool changed: %v", err)
			}
			pooled, err := openRequestDB(cfg, primary, "127.0.0.2:6432")
			if err != nil {
				t.Fatal(err)
			}
			defer pooled.Close()
			if pooled == primary || pooled.Stats().MaxOpenConnections != want || primary.Stats().MaxOpenConnections != 4 {
				t.Fatalf("unexpected limits: app %d, primary %d", pooled.Stats().MaxOpenConnections, primary.Stats().MaxOpenConnections)
			}
			if cfg.DBHost != "127.0.0.1" || cfg.DBPort != "5432" {
				t.Fatal("direct connection settings were overwritten")
			}
		})
	}
}
