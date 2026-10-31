package isureal

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
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
		// セッション確認・利用者・投稿一覧を selectWithSession の 1 文（接続 1 本・1 往復）で取得する。
		// 参照実装は image_objects と JOIN して「画像がある投稿」だけを出していたが、その判定は SQL では行わず、
		// 下で画像を集めたあとに画像が見つからなかった投稿を外す (結果は同じ)。SQL で EXISTS を引くと、投稿ごとに
		// image_objects の索引を 1 回ずつ引き、export の読み取りの約 6 割 (実測 1 回 331 件で 206 ページを disk から) を占めていた。
		// 投稿画像は isureal_create_post で投稿と同じトランザクションで作られ、投稿画像を消す処理は無いので、実際に外れる投稿は無い。
		var exportRows []struct {
			SessRegion *string `db:"sess_region"`
			UserFound  bool    `db:"user_found"`
			PostFound  bool    `db:"post_found"`
			PostWithAuthor
		}
		if err := selectWithSession(c, h, &exportRows, func() (int, *string) {
			if len(exportRows) == 0 {
				return 0, nil
			}
			return len(exportRows), exportRows[0].SessRegion
		}, `
   SELECT s.sess_region, u."author.id" IS NOT NULL AS user_found, p.id IS NOT NULL AS post_found,
          `+postWithAuthorColumnsCoalesced("r")+`
   FROM {session}
   LEFT JOIN LATERAL (
     SELECT `+userColumnsAs("u", "author")+`
     FROM users u
     WHERE (s.sess_region IS NULL OR s.sess_region = $1)
       AND u.id = s.sess_user_id AND u.status = 'active'
   ) u ON true
   LEFT JOIN LATERAL (
     SELECT `+postColumns+`
     FROM posts p
     JOIN posting_windows w ON w.id = p.posting_window_id
     WHERE u."author.id" IS NOT NULL AND p.author_id = s.sess_user_id AND p.deleted_at IS NULL
     ORDER BY p.posted_at, p.id
   ) p ON true
   CROSS JOIN LATERAL (SELECT p.*, u.*) r
   ORDER BY p.posted_at, p.id
  `, h.Config.Region); err != nil {
			return err
		}
		if len(exportRows) == 0 || !exportRows[0].UserFound {
			return NewAPIError(http.StatusNotFound, "user_not_found")
		}
		author := exportRows[0].Author
		if err := author.setImageKey(); err != nil {
			return err
		}
		posts := make([]Post, 0, len(exportRows))
		for _, row := range exportRows {
			if !row.PostFound {
				continue
			}
			posts = append(posts, row.Post)
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
			var rows []exportImageRow
			if err := h.DB.SelectContext(ctx, &rows, `
    SELECT i.id, i.data, i.stored_region, i.created_at FROM unnest($1::text[], $2::timestamptz[]) AS k(id, created_at)
    JOIN image_objects i ON i.id = k.id AND i.created_at = k.created_at
   `, missingIDs, missingAt); err != nil {
				return err
			}
			rest := make([]string, 0)
			seen := make(map[string]bool, len(rows))
			for _, row := range rows {
				seen[row.ID] = true
			}
			for _, id := range missingIDs {
				if !seen[id] {
					rest = append(rest, id)
				}
			}
			if len(rest) > 0 {
				// 固定の SQL にして、件数ごとに文が変わって Prepare の往復が増えるのを避ける。
				// sqlx の Select は受け取るスライスを 0 長にしてから詰めるので、別のスライスに読んで足す
				// (同じ rows に読むと 1 回目の結果が消え、その投稿が export から落ちる。#167)。
				var more []exportImageRow
				if err := h.DB.SelectContext(ctx, &more, `SELECT id, data, stored_region, created_at FROM image_objects WHERE id = ANY($1)`, rest); err != nil {
					return err
				}
				rows = append(rows, more...)
			}
			region := h.Config.Region
			if err := collectExportImages(rows, images, func(id, storedRegion string) ([]byte, error) {
				return loadStoredImage(id, storedRegion, region)
			}, func(row exportImageRow) {
				imageDataStore.put(row.ID, row.Data)
				saveInitialImageToDisk(row.ID, row.CreatedAt, row.Data)
			}); err != nil {
				return err
			}
		}

		// 画像が見つからなかった投稿は出さない (参照実装の JOIN image_objects と同じ)。
		kept := posts[:0]
		for _, post := range posts {
			if _, ok := images[post.ImageID]; ok {
				kept = append(kept, post)
			}
		}
		posts = kept

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

// exportImageRow は export で引く image_objects の 1 行。
type exportImageRow struct {
	ID           string    `db:"id"`
	Data         []byte    `db:"data"`
	StoredRegion *string   `db:"stored_region"`
	CreatedAt    time.Time `db:"created_at"`
}

// collectExportImages は rows の画像本体を images に入れる。DB に本体がある行は先に全部入れて onLocal を呼び、
// 本体が stored_region のサーバーのディスクにある行 (image_store.go) だけを並行して fetch で取る。
// images への書き込みは、goroutine を始める前の同期部分と、mu を取った goroutine だけにする
// (以前は goroutine の書き込みと後続行の同期書き込みが並び、concurrent map writes で落ちえた。#167)。
func collectExportImages(rows []exportImageRow, images map[string][]byte, fetch func(id, storedRegion string) ([]byte, error), onLocal func(exportImageRow)) error {
	remote := make([]exportImageRow, 0, len(rows))
	for _, row := range rows {
		if row.StoredRegion == nil || len(row.Data) > 0 {
			images[row.ID] = row.Data
			onLocal(row)
			continue
		}
		remote = append(remote, row)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var fetchErr error
	for _, row := range remote {
		wg.Add(1)
		go func(id, storedRegion string) {
			defer wg.Done()
			data, err := fetch(id, storedRegion)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fetchErr = err
				return
			}
			images[id] = data
		}(row.ID, *row.StoredRegion)
	}
	wg.Wait()
	return fetchErr
}
