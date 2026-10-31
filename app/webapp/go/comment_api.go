package isureal

import (
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type createCommentRequest struct {
	PostID string `param:"postId" json:"-"`
	Body   string `json:"body"`
}

type createCommentResponse struct {
	ID        string                   `json:"id"`
	Author    relationshipUserResponse `json:"author"`
	Body      string                   `json:"body"`
	CreatedAt time.Time                `json:"createdAt"`
}

func newCommentResponse(comment Comment, user User) createCommentResponse {
	return createCommentResponse{ID: comment.ID, Author: newRelationshipUserResponse(user), Body: comment.Body, CreatedAt: comment.CreatedAt}
}

func createCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Body, 1, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_interaction")
		}
		id := newTimeID()
		var row interactionRow
		if err := callWithSession(c, h, &row, func() *string { return row.SessRegion }, `isureal_create_comment({user}, $1, $2, $3)`, input.PostID, id, input.Body); err != nil {
			return err
		}
		if err := row.err(); err != nil {
			return err
		}
		user, err := row.user()
		if err != nil {
			return err
		}
		comment := Comment{ID: id, PostID: input.PostID, AuthorID: user.ID, Body: input.Body, CreatedAt: *row.CreatedAt}
		return c.JSON(http.StatusCreated, newCommentResponse(comment, user))
	}
}

type getCommentsRequest struct {
	PostID         string     `param:"postId"`
	AfterCreatedAt *time.Time `query:"afterCreatedAt"`
	AfterID        string     `query:"afterId"`
}

type getCommentsResponse struct {
	Items   []createCommentResponse `json:"items"`
	HasMore bool                    `json:"hasMore"`
}

func getCommentsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getCommentsRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.AfterCreatedAt == nil) != (input.AfterID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		// セッションの確認・権限の判定 (isureal_post_access)・一覧の取得を 1 文・接続 1 本で行う (#82)。
		// 権限が無いか一覧が空なら番兵の行 (found = false) が 1 行返る。
		var rows []struct {
			SessRegion *string `db:"sess_region"`
			Allowed    bool    `db:"allowed"`
			Found      bool    `db:"found"`
			CommentWithAuthor
		}
		if err := selectWithSession(c, h, &rows, func() (int, *string) {
			if len(rows) == 0 {
				return 0, nil
			}
			return len(rows), rows[0].SessRegion
		}, `
   SELECT s.sess_region, a.can_comment AS allowed, r.id IS NOT NULL AS found,
          COALESCE(r.id, '') AS id, COALESCE(r.post_id, '') AS post_id, COALESCE(r.author_id, '') AS author_id,
          COALESCE(r.body, '') AS body, COALESCE(r.created_at, 'epoch') AS created_at, r.deleted_at,
          `+userColumnsCoalesced("r", "author")+`
   FROM {session}
   CROSS JOIN LATERAL isureal_post_access(s.sess_user_id, $1) a
   LEFT JOIN LATERAL (
     SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, `+userColumnsAs("u", "author")+`
     FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
     WHERE a.can_comment AND c.deleted_at IS NULL AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3))
     ORDER BY c.created_at,c.id LIMIT 51
   ) r ON true
   ORDER BY r.created_at, r.id
  `, input.PostID, input.AfterCreatedAt, input.AfterID); err != nil {
			return err
		}
		if len(rows) == 0 || !rows[0].Allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		comments := make([]CommentWithAuthor, 0, len(rows))
		for _, row := range rows {
			if !row.Found {
				continue
			}
			comment := row.CommentWithAuthor
			if err := comment.Author.setImageKey(); err != nil {
				return err
			}
			comments = append(comments, comment)
		}
		plan := newSignaturePlan()
		for _, comment := range comments {
			plan.addProfile(comment.Author)
		}
		plan.run()
		items := make([]createCommentResponse, 0, len(comments))
		for _, comment := range comments {
			items = append(items, newCommentResponse(comment.Comment, comment.Author))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getCommentsResponse{Items: items, HasMore: hasMore})
	}
}

type deleteCommentRequest struct {
	CommentID string `param:"commentId" json:"-"`
}

func deleteCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		var result string
		if err := h.DB.GetContext(ctx, &result, `SELECT isureal_delete_comment($1, $2)`, actor, input.CommentID); err != nil {
			return err
		}
		switch result {
		case "ok":
		case "not_found":
			return NewAPIError(http.StatusNotFound, "interaction_not_found")
		case "forbidden":
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		default:
			return fmt.Errorf("unexpected delete comment result: %s", result)
		}
		return c.NoContent(http.StatusNoContent)
	}
}
