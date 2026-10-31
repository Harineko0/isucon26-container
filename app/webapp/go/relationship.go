package isureal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type FriendRequest struct {
	ID          string    `db:"id"`
	RequesterID string    `db:"requester_id"`
	RecipientID string    `db:"recipient_id"`
	Status      string    `db:"status"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

func getFriendRequest(ctx context.Context, db sqlx.QueryerContext, id string) (FriendRequest, error) {
	var request FriendRequest
	err := sqlx.GetContext(ctx, db, &request, `SELECT id, requester_id, recipient_id, status, created_at, updated_at FROM friend_requests WHERE id=$1`, id)
	return request, err
}

func beginRelationshipTx(ctx context.Context, db *sqlx.DB, actor, target string) (*sqlx.Tx, error) {
	if actor == target || target == "" {
		return nil, NewAPIError(http.StatusBadRequest, "invalid_relationship")
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, id := range []string{min(actor, target), max(actor, target)} {
		var lockedID string
		if err := tx.GetContext(ctx, &lockedID, `SELECT id FROM users WHERE id=$1 AND status='active' FOR NO KEY UPDATE`, id); err != nil {
			tx.Rollback()
			if errors.Is(err, sql.ErrNoRows) {
				return nil, NewAPIError(http.StatusNotFound, "relationship_not_found")
			}
			return nil, err
		}
	}
	return tx, nil
}

func isBlocked(ctx context.Context, db sqlx.QueryerContext, actor, target string) (bool, error) {
	var blocked bool
	err := sqlx.GetContext(ctx, db, &blocked, `SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))`, actor, target)
	return blocked, err
}

func insertFriendship(ctx context.Context, tx *sqlx.Tx, request FriendRequest) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id) VALUES ($1,$2,$3,$4)`, uuid.NewString(), min(request.RequesterID, request.RecipientID), max(request.RequesterID, request.RecipientID), request.ID)
	return err
}

func requireRelationshipChange(result sql.Result, err error, status int, code string) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return NewAPIError(status, code)
	}
	return nil
}

type relationshipResult struct {
	SessRegion *string    `db:"sess_region"`
	Result     string     `db:"o_result"`
	ID         *string    `db:"o_id"`
	Status     *string    `db:"o_status"`
	UpdatedAt  *time.Time `db:"o_updated_at"`
}

// runRelationship は 2 人をロックして行う関係の変更を DB 関数で 1 往復で行う。
func runRelationship(c *echo.Context, h *HandlerContext, action, target string) (relationshipResult, error) {
	var row relationshipResult
	actor := GetSession(c).UserID
	if actor == target || target == "" {
		return row, NewAPIError(http.StatusBadRequest, "invalid_relationship")
	}
	if err := callWithSession(c, h, &row, func() *string { return row.SessRegion }, `isureal_relationship($1, {user}, $2, $3)`, action, target, uuid.NewString()); err != nil {
		return row, err
	}
	return row, relationshipResultError(row.Result)
}

func relationshipResultError(result string) error {
	switch result {
	case "ok":
		return nil
	case "bad":
		return NewAPIError(http.StatusBadRequest, "invalid_relationship")
	case "not_found":
		return NewAPIError(http.StatusNotFound, "relationship_not_found")
	case "conflict":
		return NewAPIError(http.StatusConflict, "relationship_conflict")
	case "forbidden":
		return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
	default:
		return fmt.Errorf("unexpected relationship result: %s", result)
	}
}
