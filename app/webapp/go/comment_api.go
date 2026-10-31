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
		ctx := c.Request().Context()
		// 権限の判定と一覧の取得を並行に行い、権限がなければ一覧は捨てる。
		var comments []CommentWithAuthor
		var listErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			comments, listErr = selectCommentsWithAuthor(ctx, h.DB, `AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3)) ORDER BY c.created_at,c.id LIMIT 51`, input.PostID, input.AfterCreatedAt, input.AfterID)
		}()
		_, allowed, err := postAccess(ctx, h.DB, GetSession(c).UserID, input.PostID)
		<-done
		if err != nil {
			return err
		}
		if !allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if listErr != nil {
			return listErr
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
