package isureal

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type exportPostResponse struct {
	createPostResponse
	ImagePath string `json:"imagePath"`
}

func exportPostsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		// 投稿一覧は利用者の取得に依存しないので並行に取り、往復を 1 回分に重ねる。
		// 投稿画像の created_at は posted_at と同じ(isureal_create_post)なので、EXISTS も (id, created_at) で引いて
		// image_objects のパーティションを 1 つに絞る。
		var posts []Post
		postsDone := make(chan error, 1)
		go func() {
			postsDone <- h.DB.SelectContext(ctx, &posts, `
   SELECT `+postColumns+`
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
     AND EXISTS (SELECT 1 FROM image_objects i WHERE i.id = p.image_id AND i.created_at = p.posted_at)
   ORDER BY p.posted_at, p.id
  `, userID)
		}()
		author, err := getUser(ctx, h.DB, userID)
		postsErr := <-postsDone
		if err != nil {
			return err
		}
		if postsErr != nil {
			return postsErr
		}
		// 画像の本体は不変なので、手元にないものだけ DB から取る。
		images := make(map[string][]byte, len(posts))
		missingIDs := make([]string, 0)
		missingAt := make([]time.Time, 0)
		for _, post := range posts {
			if data, ok := imageDataStore.get(post.ImageID); ok {
				images[post.ImageID] = data
			} else {
				missingIDs = append(missingIDs, post.ImageID)
				missingAt = append(missingAt, post.PostedAt)
			}
		}
		if len(missingIDs) > 0 {
			// 投稿画像の created_at は投稿時刻と同じなので、(id, created_at) で引いてパーティションを絞る。
			// 一致しないものがあれば id だけで引き直す。
			var rows []struct {
				ID   string `db:"id"`
				Data []byte `db:"data"`
			}
			if err := h.DB.SelectContext(ctx, &rows, `
    SELECT i.id, i.data FROM unnest($1::text[], $2::timestamptz[]) AS k(id, created_at)
    JOIN image_objects i ON i.id = k.id AND i.created_at = k.created_at
   `, missingIDs, missingAt); err != nil {
				return err
			}
			for _, row := range rows {
				images[row.ID] = row.Data
				imageDataStore.put(row.ID, row.Data)
			}
			rest := make([]string, 0)
			for _, id := range missingIDs {
				if _, ok := images[id]; !ok {
					rest = append(rest, id)
				}
			}
			if len(rest) > 0 {
				// 固定の SQL にして、件数ごとに文が変わって Prepare の往復が増えるのを避ける。
				rows = rows[:0]
				if err := h.DB.SelectContext(ctx, &rows, `SELECT id, data FROM image_objects WHERE id = ANY($1)`, rest); err != nil {
					return err
				}
				for _, row := range rows {
					images[row.ID] = row.Data
					imageDataStore.put(row.ID, row.Data)
				}
			}
		}

		plan := newSignaturePlan()
		plan.addProfile(author)
		for _, post := range posts {
			plan.add(author, post.ImageID)
		}
		plan.run()

		// 失敗しうる処理(DB・署名)はここまでに済ませてあるので、ZIP は応答へ直接書き出す
		// (ZIP 全体をもう 1 つバッファに組み立てない)。出力するバイト列は従来と同じ。
		header := c.Response().Header()
		header.Set(echo.HeaderContentDisposition, `attachment; filename="isureal-posts.zip"`)
		header.Set(echo.HeaderCacheControl, "no-store")
		header.Set(echo.HeaderContentType, "application/zip")
		c.Response().WriteHeader(http.StatusOK)
		return writeExportZip(c.Response(), posts, images, author)
	}
}

// writeExportZip は images/NNNNNN.jpg (投稿順) と posts.json を無圧縮の ZIP として w に書く。
func writeExportZip(w io.Writer, posts []Post, images map[string][]byte, author User) error {
	writer := zip.NewWriter(w)
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
	return writer.Close()
}
