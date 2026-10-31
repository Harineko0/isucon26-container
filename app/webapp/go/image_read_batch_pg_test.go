package isureal

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func TestImageReadBatchOnPostgres(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("set ISUREAL_TEST_DSN to local PostgreSQL 16")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("test database must be local")
	}
	db, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// All fixture tables are TEMP and this test owns exactly one connection.
	db.SetMaxOpenConns(1)
	db.MustExec(`CREATE TEMP TABLE users(id text PRIMARY KEY, secret_key_hash text, status text, profile_image_id text);
CREATE TEMP TABLE posts(id text PRIMARY KEY, image_id text, author_id text, deleted_at timestamptz);
CREATE TEMP TABLE realmojis(image_id text, reactor_id text, post_id text, replaced_at timestamptz, deleted_at timestamptz);
CREATE TEMP TABLE image_objects(id text, owner_user_id text, media_type text, created_at timestamptz, data bytea, stored_region text) PARTITION BY RANGE(created_at);
CREATE TEMP TABLE image_objects_202601 PARTITION OF image_objects FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
INSERT INTO users VALUES ('owner', repeat('ab',32), 'active', 'profile'), ('author',repeat('cd',32),'active',NULL), ('deleted',repeat('ef',32),'deleted',NULL);
INSERT INTO posts VALUES ('post','posted','owner',NULL), ('target','unused','author',NULL), ('deleted-post','deleted-image','owner',now());
INSERT INTO realmojis VALUES ('real','owner','target',NULL,NULL), ('replaced','owner','target',now(),NULL), ('dead-real','owner','deleted-post',NULL,NULL);
INSERT INTO image_objects SELECT id, owner, 'image/jpeg', '2026-01-02'::timestamptz, convert_to(id,'UTF8'), NULL
FROM (VALUES ('profile','owner'), ('posted','owner'), ('real','owner'), ('replaced','owner'), ('dead-real','owner'), ('deleted-image','owner'), ('deleted-owner','deleted')) v(id,owner);`)
	h := &HandlerContext{DB: db, IPBans: newIPBanList(), Config: &Config{Region: "asia"}}
	requests := []imageReadRequest{}
	for _, id := range []string{"profile", "posted", "real", "replaced", "dead-real", "deleted-image", "deleted-owner", "missing"} {
		for _, hasData := range []bool{false, true} {
			requests = append(requests, imageReadRequest{id: id, hasData: hasData})
		}
	}
	for _, rel := range []string{"image_objects", "image_objects_202601"} {
		results := loadImageReadBatch(t.Context(), h, rel, requests)
		for i, request := range requests {
			var want imageObject
			err := db.GetContext(t.Context(), &want, imageQueryFor(rel), request.id, request.hasData)
			if !errors.Is(results[i].err, err) || (err == nil && !reflect.DeepEqual(results[i].object, want)) {
				t.Fatalf("%s %s cached=%t: batch=%+v single=%+v err=%v", rel, request.id, request.hasData, results[i], want, err)
			}
		}
		if !results[0].object.Allowed || !results[2].object.Allowed || !results[4].object.Allowed || results[6].object.Allowed || results[8].object.Allowed || results[10].object.Allowed || !errors.Is(results[12].err, sql.ErrNoRows) {
			t.Fatal("fixture must cover allowed/invalid/deleted images")
		}
	}
	// Filter a banned IP after acquiring the shared DB connection; ordinality
	// still has to route the other, duplicate-ID request to the correct caller.
	h.IPBans.snapshot.Store(&ipBanSnapshot{bans: map[string]time.Time{"banned": time.Now().Add(time.Hour)}})
	got := loadImageReadBatch(t.Context(), h, "image_objects_202601", []imageReadRequest{
		{id: "posted", ip: "banned"}, {id: "posted", hasData: true, ip: "allowed"}, {id: "missing"},
	})
	if apiErr, ok := got[0].err.(*APIError); !ok || apiErr.Code != "ip_banned" {
		t.Fatal("banned request reached the image query")
	}
	if got[1].err != nil || !got[1].object.Allowed || len(got[1].object.Data) != 0 || !errors.Is(got[2].err, sql.ErrNoRows) {
		t.Fatalf("bad result routing after BAN: %+v", got)
	}

	oldStore := imageDataStore
	imageDataStore = newImageDataCache()
	t.Cleanup(func() { imageDataStore = oldStore })
	h.imageReads = &imageReads{load: func(ctx context.Context, rel string, requests []imageReadRequest) []imageReadResult {
		return loadImageReadBatch(ctx, h, rel, requests)
	}}
	for _, id := range []string{"profile", "posted", "real", "replaced", "deleted-owner"} {
		imagePartitions.Store(id, "image_objects_202601")
		t.Cleanup(func() { imagePartitions.Delete(id) })
	}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/images/:imageId", getImageHandler(h))
	key, _ := imageSigningKey(strings.Repeat("ab", 32))
	get := func(id string, signatureValid bool, status int) {
		t.Helper()
		target := signedImageURL(User{ImageKey: key}, id)
		if !signatureValid {
			u, _ := url.Parse(target)
			q := u.Query()
			q.Set("sig", strings.Repeat("A", 43))
			u.RawQuery = q.Encode()
			target = u.String()
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != status || (status == 200 && !bytes.Equal(rec.Body.Bytes(), []byte(id))) {
			t.Fatalf("GET %s: %d %s, want %d", id, rec.Code, rec.Body.String(), status)
		}
	}
	get("posted", true, 200)
	get("posted", false, 403)
	get("real", true, 200)
	get("replaced", true, 403)
	get("deleted-owner", true, 403)
	db.MustExec(`UPDATE posts SET deleted_at=now() WHERE id='post'; UPDATE realmojis SET replaced_at=now() WHERE image_id='real'`)
	get("posted", true, 403)
	get("real", true, 403)
	get("profile", true, 200)
	db.MustExec(`UPDATE users SET status='deleted' WHERE id='owner'`)
	get("profile", true, 403)
}
