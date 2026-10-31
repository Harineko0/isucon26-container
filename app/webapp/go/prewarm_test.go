package isureal

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// TestPrewarmPlan は候補の振り分けを確かめる: 優先度順に予算まで 'buffer'、残りは 'read'、
// 月パーティションの主キーは 'buffer' 側に、heap は 'read' 側に展開され、無い relation と空の relation は飛ばす。
func TestPrewarmPlan(t *testing.T) {
	rels := []prewarmRelation{
		{OID: 1, Name: "users", Blocks: 10},
		{OID: 2, Name: "posts_pkey", Blocks: 50},
		{OID: 3, Name: "posts", Blocks: 60},
		{OID: 4, Name: "image_objects_202605_pkey", Blocks: 5},
		{OID: 5, Name: "image_objects_202605", Blocks: 100},
		{OID: 6, Name: "image_objects_202701_pkey", Blocks: 0},
		{OID: 7, Name: "image_objects_20261028_p1", Blocks: 3}, // 月パーティションではない
		{OID: 8, Name: "notifications", Blocks: 1000},           // 候補にない
		{OID: 9, Name: "realmojis", Blocks: 70},
		{OID: 10, Name: "realmojis_post_id", Blocks: 22},
		{OID: 11, Name: "realmojis_one_active", Blocks: 22},
		{OID: 12, Name: "realmojis_pkey", Blocks: 50},
		{OID: 13, Name: "comments_pkey", Blocks: 30},
	}
	buffer, read := prewarmPlan(rels, 147)
	names := func(list []prewarmRelation) []string {
		out := make([]string, 0, len(list))
		for _, r := range list {
			out = append(out, r.Name)
		}
		return out
	}
	wantBuffer := []string{"users", "posts_pkey", "posts", "realmojis_post_id", "image_objects_202605_pkey"}
	wantRead := []string{"realmojis_one_active", "realmojis", "image_objects_202605"}
	if got := names(buffer); !equalStrings(got, wantBuffer) {
		t.Errorf("buffer = %v, want %v", got, wantBuffer)
	}
	if got := names(read); !equalStrings(got, wantRead) {
		t.Errorf("read = %v, want %v", got, wantRead)
	}
	// 候補の名前の綴りを DB の名前と照らす前提として、"*" の展開以外は重複が無いこと。
	seen := map[string]bool{}
	for _, name := range prewarmCandidates {
		if seen[name] {
			t.Errorf("duplicate candidate %q", name)
		}
		seen[name] = true
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPrewarmDBOnPostgres は本物の PostgreSQL で CREATE EXTENSION pg_prewarm と pg_prewarm() が通り、
// 候補の名前が実在の表・索引と一致する (綴り違いで飛ばしていない) ことを確かめる。
// ISUREAL_TEST_DSN (001〜008 を流した空の DB) が無ければ飛ばす。手順は sql/tests/prewarm.sh。
func TestPrewarmDBOnPostgres(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set (see app/webapp/sql/tests/prewarm.sh)")
	}
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := sqlx.NewDb(stdlib.OpenDB(*connConfig), "pgx")
	defer db.Close()
	ctx := context.Background()

	// 候補 ("*" 以外) は全部 DB に実在すること。
	var missing []string
	if err := db.SelectContext(ctx, &missing, `
   SELECT name FROM unnest($1::text[]) AS name
   WHERE name NOT LIKE 'image_objects_*%'
     AND NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                     WHERE n.nspname = 'public' AND c.relname = name)`, prewarmCandidates); err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("candidates not in the schema: %v", missing)
	}

	// 1 ページでも読むものがあるように行を入れる。
	for _, statement := range []string{
		`INSERT INTO users (id, username, secret_key_hash, display_name, region) VALUES ('pw1', 'prewarm', 'x', 'Prewarm', 'asia') ON CONFLICT DO NOTHING`,
		`INSERT INTO image_objects (id, owner_user_id, media_type, data, created_at) VALUES ('pwimg', 'pw1', 'image/jpeg', '\x00', '2026-05-01T00:00:00Z') ON CONFLICT DO NOTHING`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	prewarmLastDone.Store(0)
	prewarmDB(db, "test")
	if prewarmLastDone.Load() == 0 {
		t.Fatal("prewarmDB did not finish without errors (see stderr)")
	}
	var installed bool
	if err := db.GetContext(ctx, &installed, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_prewarm')`); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Error("pg_prewarm extension is not installed")
	}
	var n int64
	if err := db.GetContext(ctx, &n, `SELECT pg_prewarm('users', 'buffer')`); err != nil || n == 0 {
		t.Errorf("pg_prewarm(users) = %d, %v", n, err)
	}
	// 2 回目は debounce で飛ばす (完了時刻が変わらない)。
	last := prewarmLastDone.Load()
	time.Sleep(10 * time.Millisecond)
	prewarmDB(db, "test-again")
	if prewarmLastDone.Load() != last {
		t.Error("second prewarmDB within prewarmDebounce should be skipped")
	}
}
