package isureal

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

// legacyExportResponse は変更前のハンドラの出力部分 (bytes.Buffer に組み立てて Blob) をそのまま再現する。
func legacyExportResponse(c *echo.Context, posts []Post, images map[string][]byte, author User) error {
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	defer writer.Close()
	items := make([]exportPostResponse, 0, len(posts))
	for _, post := range posts {
		imagePath := fmt.Sprintf("images/%06d.jpg", len(items)+1)
		image, err := writer.CreateHeader(&zip.FileHeader{Name: imagePath, Method: zip.Store})
		if err != nil {
			return err
		}
		if _, err := image.Write(images[post.ImageID]); err != nil {
			return err
		}
		items = append(items, exportPostResponse{createPostResponse: newPostResponse(post, author), ImagePath: imagePath})
	}
	manifest, err := writer.Create("posts.json")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(manifest).Encode(items); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="isureal-posts.zip"`)
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.Blob(http.StatusOK, "application/zip", body.Bytes())
}

// streamingExportResponse は変更後のハンドラの出力部分と同じ手順で応答へ直接書く。
func streamingExportResponse(c *echo.Context, posts []Post, images map[string][]byte, author User) error {
	header := c.Response().Header()
	header.Set(echo.HeaderContentDisposition, `attachment; filename="isureal-posts.zip"`)
	header.Set(echo.HeaderCacheControl, "no-store")
	header.Set(echo.HeaderContentType, "application/zip")
	c.Response().WriteHeader(http.StatusOK)
	return writeExportZip(c.Response(), posts, images, author)
}

func exportFixture(n int) ([]Post, map[string][]byte, User) {
	author := User{
		ID: "11111111-1111-1111-1111-111111111111", Username: "alice", DisplayName: "Alice",
		Bio: "bio", Region: "asia", AccountType: "normal", Status: "active",
		ImageKey: bytes.Repeat([]byte{7}, 32),
	}
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	locality := "Tokyo"
	posts := make([]Post, 0, n)
	images := make(map[string][]byte, n)
	for i := range n {
		imageID := fmt.Sprintf("aaaaaaaa-0000-0000-0000-%012d", i)
		post := Post{
			ID: fmt.Sprintf("bbbbbbbb-0000-0000-0000-%012d", i), AuthorID: author.ID,
			PostingWindowID: fmt.Sprintf("w%d", i), ImageID: imageID, Caption: fmt.Sprintf("caption %d", i),
			Visibility:     "public",
			PostLocation:   PostLocation{Kind: "none"},
			PostedAt:       base.Add(time.Duration(i) * 24 * time.Hour),
			OnTimeDeadline: base.Add(time.Duration(i)*24*time.Hour + time.Duration(i%3-1)*time.Minute),
		}
		if i%2 == 1 {
			post.PostLocation = PostLocation{Kind: "approximate", Locality: &locality}
		}
		posts = append(posts, post)
		images[imageID] = bytes.Repeat([]byte{byte(i)}, 1000+i*37)
	}
	return posts, images, author
}

func runExport(t *testing.T, write func(*echo.Context, []Post, map[string][]byte, User) error, posts []Post, images map[string][]byte, author User) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/me/export", nil), rec)
	if err := write(c, posts, images, author); err != nil {
		t.Fatalf("write export: %v", err)
	}
	return rec
}

func TestWriteExportZipMatchesLegacyResponse(t *testing.T) {
	tests := []struct {
		name  string
		posts int
	}{
		{name: "no posts", posts: 0},
		{name: "one post", posts: 1},
		{name: "many posts", posts: 40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posts, images, author := exportFixture(tt.posts)
			var legacy, streamed *httptest.ResponseRecorder
			// 署名の exp は時刻の区切りで変わるので、区切りをまたいだときだけやり直す。
			for range 3 {
				legacy = runExport(t, legacyExportResponse, posts, images, author)
				streamed = runExport(t, streamingExportResponse, posts, images, author)
				if bytes.Equal(legacy.Body.Bytes(), streamed.Body.Bytes()) {
					break
				}
			}
			if !bytes.Equal(legacy.Body.Bytes(), streamed.Body.Bytes()) {
				t.Fatalf("body differs: legacy %d bytes, streamed %d bytes", legacy.Body.Len(), streamed.Body.Len())
			}
			if legacy.Code != streamed.Code {
				t.Errorf("status: legacy %d, streamed %d", legacy.Code, streamed.Code)
			}
			for _, key := range []string{echo.HeaderContentType, echo.HeaderContentDisposition, echo.HeaderCacheControl, echo.HeaderContentLength} {
				if got, want := streamed.Header().Get(key), legacy.Header().Get(key); got != want {
					t.Errorf("header %s: streamed %q, legacy %q", key, got, want)
				}
			}

			reader, err := zip.NewReader(bytes.NewReader(streamed.Body.Bytes()), int64(streamed.Body.Len()))
			if err != nil {
				t.Fatalf("read zip: %v", err)
			}
			if len(reader.File) != tt.posts+1 {
				t.Fatalf("files: got %d, want %d", len(reader.File), tt.posts+1)
			}
			for i, file := range reader.File[:tt.posts] {
				if want := fmt.Sprintf("images/%06d.jpg", i+1); file.Name != want {
					t.Errorf("file %d: got %q, want %q", i, file.Name, want)
				}
			}
			if last := reader.File[tt.posts].Name; last != "posts.json" {
				t.Errorf("last file: got %q, want posts.json", last)
			}
			manifest, err := reader.File[tt.posts].Open()
			if err != nil {
				t.Fatalf("open manifest: %v", err)
			}
			defer manifest.Close()
			body, err := io.ReadAll(manifest)
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			if tt.posts == 0 && string(body) != "[]\n" {
				t.Errorf("empty manifest: got %q, want %q", body, "[]\n")
			}
		})
	}
}
