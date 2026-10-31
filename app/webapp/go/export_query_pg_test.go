package isureal

import (
	"os"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
)

// Test the actual result set. The HTTP tests also cover deferred session
// rejection, but do not check how often the author crosses the DB connection.
func TestExportAuthorOnceOnPostgres(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("set ISUREAL_TEST_DSN to a local test database")
	}
	db, err := sqlx.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	exec := func(stmt string) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TEMP TABLE users (LIKE public.users INCLUDING DEFAULTS)`)
	exec(`CREATE TEMP TABLE posts (LIKE public.posts INCLUDING DEFAULTS)`)
	exec(`CREATE TEMP TABLE posting_windows (LIKE public.posting_windows INCLUDING DEFAULTS)`)
	exec(`INSERT INTO users (id, username, secret_key_hash, display_name, region)
	      VALUES ('exporter', 'exporter', repeat('ab', 32), 'Alice', 'asia')`)

	read := func(userID string) []exportPostRow {
		t.Helper()
		const source = "(SELECT $2::text AS sess_user_id, NULL::text AS sess_region) s"
		var rows []exportPostRow
		if err := tx.SelectContext(ctx, &rows, strings.Replace(exportPostsQuery, "{session}", source, 1), "asia", userID); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := read("missing"); len(rows) != 1 || rows[0].UserFound || rows[0].PostFound {
		t.Fatalf("missing user: %+v", rows)
	}
	if rows := read("exporter"); len(rows) != 1 || !rows[0].UserFound || rows[0].PostFound || rows[0].Author.DisplayName != "Alice" {
		t.Fatalf("empty history: %+v", rows)
	}
	exec(`UPDATE users SET display_name = 'Revised', profile_image_id = 'profile' WHERE id = 'exporter'`)
	exec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
	      VALUES ('w', 'asia', current_date, now(), now())`)
	exec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, posted_at)
	      SELECT id, 'exporter', 'w', id, '', 'public', 'none', '2026-01-01'
	      FROM unnest(ARRAY['b','c','a']) post(id)`)
	exec(`UPDATE posts SET deleted_at = now() WHERE id = 'b'`)
	rows := read("exporter")
	if len(rows) != 2 || rows[0].ID != "a" || rows[1].ID != "c" || !rows[0].PostFound || !rows[1].PostFound {
		t.Fatalf("ordered history: %+v", rows)
	}
	author := rows[0].Author
	if author.DisplayName != "Revised" || author.ProfileImageID == nil || *author.ProfileImageID != "profile" || author.SecretKeyHash != strings.Repeat("ab", 32) {
		t.Fatalf("first author: %+v", author)
	}
	if got := rows[1].Author; got.ID != "" || got.Username != "" || got.SecretKeyHash != "" || got.DisplayName != "" ||
		got.ProfileImageID != nil || got.Bio != "" || got.Region != "" || got.AccountType != "" || got.Status != "" {
		t.Fatalf("repeated author metadata: %+v", got)
	}
}
