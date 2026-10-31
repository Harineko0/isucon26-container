package isureal

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
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
		author, err := getUser(ctx, h.DB, userID)
		if err != nil {
			return err
		}
		var posts []Post
		if err := h.DB.SelectContext(ctx, &posts, `
   SELECT `+postColumns+`
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND p.deleted_at IS NULL AND EXISTS (SELECT 1 FROM image_objects i WHERE i.id = p.image_id)
   ORDER BY p.posted_at, p.id
  `, userID); err != nil {
			return err
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
				query, args, err := sqlx.In(`SELECT id, data FROM image_objects WHERE id IN (?)`, rest)
				if err != nil {
					return err
				}
				rows = rows[:0]
				if err := h.DB.SelectContext(ctx, &rows, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
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
}
