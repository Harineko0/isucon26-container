package isureal

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// TestGenericPlanTakesFewerLocksOnPostgres は #153 の plan_cache_mode = force_generic_plan で、新しい接続が
// 最初に文を実行したときの表ロック (fast-path の 16 を超えると共有のロック表に行き LWLock:LockManager で待つ) が
// 減ることを本物の PostgreSQL 16 で確かめる。既定 (auto) では接続ごとに最初の 5 回が custom plan で、
// custom plan は文中の表の索引をすべてロックする。ISUREAL_TEST_DSN が無ければ飛ばす (sql/tests/roundtrips.sh)。
func TestGenericPlanTakesFewerLocksOnPostgres(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set (see app/webapp/sql/tests/roundtrips.sh)")
	}
	ctx := context.Background()

	// 新しい接続で文を 1 回実行して計画を作らせ、2 回目 (auto では custom plan を作り直す、generic では
	// キャッシュした計画を使う) のトランザクションが持つ表ロックの数を返す。1 回目はどちらも計画を作るので同じ。
	locksOnSecondRun := func(t *testing.T, planCacheMode, query string, args ...any) int {
		t.Helper()
		connConfig, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		connConfig.RuntimeParams["plan_cache_mode"] = planCacheMode
		db := sqlx.NewDb(stdlib.OpenDB(*connConfig), "pgx")
		defer db.Close()
		db.SetMaxOpenConns(1)
		first, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		for first.Next() {
		}
		first.Close()
		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		var locks int
		if err := tx.GetContext(ctx, &locks, `SELECT count(*) FROM pg_locks
  WHERE pid = pg_backend_pid() AND locktype = 'relation' AND relation <> 'pg_locks'::regclass`); err != nil {
			t.Fatal(err)
		}
		return locks
	}

	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{"image GET on a leaf partition", imageQueryFor("image_objects_20261028"), []any{"no-such-image", true}},
		{"image GET on the parent table", imageQueryFor("image_objects"), []any{"no-such-image", true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auto := locksOnSecondRun(t, "auto", tc.query, tc.args...)
			generic := locksOnSecondRun(t, dbPlanCacheMode, tc.query, tc.args...)
			t.Logf("relation locks on a connection's 2nd run: auto (custom plan) %d, %s %d", auto, dbPlanCacheMode, generic)
			if generic >= auto {
				t.Fatalf("generic plan took %d locks, want fewer than the custom plan's %d", generic, auto)
			}
		})
	}
}
