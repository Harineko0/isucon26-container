package isureal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func postImageRequest(t *testing.T, ctx context.Context, actor string, img []byte, location PostLocation) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := map[string]string{"visibility": "public", "caption": "旅行 " + actor, "location_kind": location.Kind}
	if location.Latitude != nil {
		fields["latitude"] = strconv.FormatFloat(*location.Latitude, 'g', -1, 64)
		fields["longitude"] = strconv.FormatFloat(*location.Longitude, 'g', -1, 64)
	}
	if location.Locality != nil {
		fields["locality"] = *location.Locality
	}
	for name, value := range fields {
		if err := form.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := form.CreateFormFile("image", "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(img); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/posts", &body)
	req.Header.Set(echo.HeaderAuthorization, "Bearer post-"+actor)
	req.Header.Set(echo.HeaderContentType, form.FormDataContentType())
	return req
}

func postPipelineTestServer(t *testing.T) (*echo.Echo, *writeQueue[createPostRow], []byte) {
	t.Helper()
	db := loginTestDB(t)
	setupShardedStore(t)
	db.MustExec(`INSERT INTO posting_windows(id, region, local_date, starts_at, on_time_deadline)
		VALUES ('open','asia',date '2026-10-31',now()-interval '1 minute',now()+interval '1 hour')`)
	for _, id := range []string{"u1", "u2", "u3", "u4", "u6", "u7"} {
		digest := sha256.Sum256([]byte("post-" + id))
		token := hex.EncodeToString(digest[:])
		db.MustExec(`INSERT INTO login_sessions(id,user_id,token_hash) VALUES ($1,$1,$2)`, id, token)
		knownSessions.Store(token, Session{UserID: id, TokenHash: token, Region: "asia"})
		t.Cleanup(func() { knownSessions.Delete(token) })
	}
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	queue := &writeQueue[createPostRow]{db: db, sql: createPostSQL}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	auth := []echo.MiddlewareFunc{SessionMiddleware(h, true), RegionMiddleware(h)}
	e.POST("/api/posts", createPostHandlerWithQueue(h, queue), auth...)
	e.GET("/api/posts/me/current", getMyCurrentPostHandler(h), auth...)

	var img bytes.Buffer
	if err := jpeg.Encode(&img, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	return e, queue, img.Bytes()
}

func TestCreatePostPipelineOnPostgres(t *testing.T) {
	e, queue, img := postPipelineTestServer(t)
	db := queue.db
	lat, lon := 35.678901234567, -139.789012345678
	locality := "新宿・東京"
	none := PostLocation{Kind: "none"}
	cases := []struct {
		actor    string
		location PostLocation
		status   int
		code     string
	}{
		{"u1", PostLocation{Kind: "exact", Latitude: &lat, Longitude: &lon}, 201, ""},
		{"u1", none, 409, "post_slot_unavailable"},
		{"u2", none, 401, "invalid_session"},
		{"u3", none, 403, "region_mismatch"},
		{"u4", PostLocation{Kind: "approximate", Locality: &locality}, 201, ""},
		{"u6", none, 201, ""},
	}

	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	held, err := db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var handlers sync.WaitGroup
	defer func() { cancel(); held.Close(); handlers.Wait() }()
	// Pipeline results arrive as text in the database session's timezone.
	if _, err := held.ExecContext(ctx, `SET TIME ZONE 'Pacific/Honolulu'`); err != nil {
		t.Fatal(err)
	}
	initial := db.Stats().WaitCount
	replies := make([]*httptest.ResponseRecorder, len(cases))
	for i, tc := range cases {
		req := postImageRequest(t, ctx, tc.actor, img, tc.location)
		replies[i] = httptest.NewRecorder()
		handlers.Go(func() { e.ServeHTTP(replies[i], req) })
		waitForQueuedWrites(t, ctx, queue, i+1, initial)
	}
	if waits := db.Stats().WaitCount - initial; waits != 1 {
		t.Fatalf("checkout waits = %d, want 1 for %d posts", waits, len(cases))
	}
	if _, err := held.ExecContext(ctx, `UPDATE login_sessions SET active=false WHERE user_id='u2';
		UPDATE users SET region='europe' WHERE id='u3'`); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	handlers.Wait()

	for i, tc := range cases {
		rec := replies[i]
		if rec.Code != tc.status {
			t.Fatalf("%s: status %d (%s), want %d", tc.actor, rec.Code, rec.Body, tc.status)
		}
		if tc.code != "" {
			var response APIErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Error != tc.code {
				t.Fatalf("%s: got %s, want %s", tc.actor, rec.Body, tc.code)
			}
			continue
		}
		var created, persisted createPostResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		read := serveRoundTrip(e, "post-"+tc.actor, http.MethodGet, "/api/posts/me/current", "")
		if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &persisted) != nil || created.ID != persisted.ID ||
			created.Author.ID != tc.actor || created.PostingWindowID != "open" || created.Caption != "旅行 "+tc.actor ||
			created.PostedAt.Format(time.RFC3339Nano) != persisted.PostedAt.Format(time.RFC3339Nano) ||
			!reflect.DeepEqual(created.Location, tc.location) || !reflect.DeepEqual(created.Location, persisted.Location) {
			t.Fatalf("%s: created %s; read %s", tc.actor, rec.Body, read.Body)
		}
		var imageID string
		if err := db.Get(&imageID, `SELECT p.image_id FROM posts p JOIN image_objects i
			ON i.id=p.image_id AND i.owner_user_id=p.author_id AND i.created_at=p.posted_at
			WHERE p.id=$1 AND i.stored_region='asia' AND octet_length(i.data)=0`, created.ID); err != nil {
			t.Fatal(err)
		}
		if data, ok := readImageFromStore(imageID); !ok || !bytes.Equal(data, img) {
			t.Fatalf("missing or different stored image for %s", created.ID)
		}
	}
	var posts, transactions, objects int
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT xmin::text), (SELECT count(*) FROM image_objects)
		FROM posts WHERE posting_window_id='open'`).Scan(&posts, &transactions, &objects); err != nil ||
		posts != 3 || transactions != 3 || objects != 3 {
		t.Fatalf("posts=%d transactions=%d objects=%d err=%v", posts, transactions, objects, err)
	}

	// Make the origin unwritable even when tests run as root. This request
	// must still commit the entire JPEG to PostgreSQL.
	invalidDir := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(invalidDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	imageStoreDir = invalidDir
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, postImageRequest(t, ctx, "u7", img, none))
	var fallback []byte
	if rec.Code != 201 {
		t.Fatalf("fallback status %d: %s", rec.Code, rec.Body)
	}
	if err := db.Get(&fallback, `SELECT i.data FROM posts p JOIN image_objects i ON p.image_id=i.id
		WHERE p.author_id='u7' AND i.stored_region IS NULL`); err != nil || !bytes.Equal(fallback, img) {
		t.Fatalf("fallback lost image: bytes=%d err=%v", len(fallback), err)
	}
}

// Once dispatched, a post occupies its gate slot until the database replies.
// The whole pipeline can be interrupted when its last client goes away.
func TestCreatePostCancellationAfterDispatch(t *testing.T) {
	originalSize := createPostGateSize["asia"]
	createPostGateSize["asia"] = 2
	t.Cleanup(func() { createPostGateSize["asia"] = originalSize })
	for _, allCanceled := range []bool{false, true} {
		name := "one canceled"
		if allCanceled {
			name = "every caller canceled"
		}
		t.Run(name, func(t *testing.T) {
			e, queue, img := postPipelineTestServer(t)
			db := queue.db
			db.SetMaxOpenConns(2)
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			// One connection holds the real user-row lock. The second is checked
			// out until both requests queue, then becomes the pipeline connection.
			blocker, err := db.BeginTxx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM users WHERE id='u1' FOR NO KEY UPDATE`); err != nil {
				t.Fatal(err)
			}
			held, err := db.Connx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := held.GetContext(ctx, &pid, `SELECT pg_backend_pid()`); err != nil {
				t.Fatal(err)
			}
			var handlers sync.WaitGroup
			defer func() { cancel(); blocker.Rollback(); held.Close(); handlers.Wait() }()
			before := db.Stats().WaitCount
			var abort [2]context.CancelFunc
			var replies [2]chan int
			for i, actor := range []string{"u1", "u2"} {
				requestCtx, stop := context.WithCancel(ctx)
				abort[i] = stop
				defer stop()
				req := postImageRequest(t, requestCtx, actor, img, PostLocation{Kind: "none"})
				replies[i] = make(chan int, 1)
				handlers.Go(func() {
					rec := httptest.NewRecorder()
					e.ServeHTTP(rec, req)
					replies[i] <- rec.Code
				})
				waitForQueuedWrites(t, ctx, queue, i+1, before)
			}
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			blocked := func() bool {
				t.Helper()
				var waiting bool
				if err := blocker.GetContext(ctx, &waiting, `SELECT pg_backend_pid()=ANY(pg_blocking_pids($1))`, pid); err != nil {
					t.Fatal(err)
				}
				return waiting
			}
			for !blocked() {
				time.Sleep(time.Millisecond)
			}
			abort[0]()
			// Both gate slots remain occupied. A third upload times out at
			// admission without queuing another database connection.
			probe, stopProbe := context.WithTimeout(ctx, 100*time.Millisecond)
			e.ServeHTTP(httptest.NewRecorder(), postImageRequest(t, probe, "u3", img, PostLocation{Kind: "none"}))
			stopProbe()
			if waits := db.Stats().WaitCount - before; waits != 1 {
				t.Fatalf("third post reached the pool before the first two finished: %d waits", waits)
			}
			select {
			case status := <-replies[0]:
				t.Fatalf("canceled post returned %d while its SQL was still blocked", status)
			default:
			}
			if allCanceled {
				abort[1]()
			} else if err := blocker.Rollback(); err != nil {
				t.Fatal(err)
			}
			for _, reply := range replies {
				select {
				case status := <-reply:
					if !allCanceled && status != http.StatusCreated {
						t.Fatalf("dispatched post returned %d after commit", status)
					}
				case <-ctx.Done():
					t.Fatal("handler did not exit after release or cancellation")
				}
			}
			if allCanceled {
				// pgx may send PostgreSQL's cancel asynchronously. The blocked
				// statement must stop even though the test still owns its lock.
				for blocked() {
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
}
