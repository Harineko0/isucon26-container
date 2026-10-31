package isureal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
)

type Comment struct {
	ID        string     `db:"id"`
	PostID    string     `db:"post_id"`
	AuthorID  string     `db:"author_id"`
	Body      string     `db:"body"`
	CreatedAt time.Time  `db:"created_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

func getComment(ctx context.Context, db sqlx.QueryerContext, id string) (Comment, error) {
	var comment Comment
	err := sqlx.GetContext(ctx, db, &comment, `SELECT id,post_id,author_id,body,created_at,deleted_at FROM comments WHERE id=$1`, id)
	return comment, err
}

type CommentWithAuthor struct {
	Comment
	Author User `db:"author"`
}

func selectCommentsWithAuthor(ctx context.Context, db sqlx.QueryerContext, conditions string, args ...any) ([]CommentWithAuthor, error) {
	var comments []CommentWithAuthor
	if err := sqlx.SelectContext(ctx, db, &comments, `
  SELECT c.id, c.post_id, c.author_id, c.body, c.created_at, c.deleted_at, `+userColumnsAs("u", "author")+`
  FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.deleted_at IS NULL `+conditions, args...); err != nil {
		return nil, err
	}
	for i := range comments {
		if err := comments[i].Author.setImageKey(); err != nil {
			return nil, err
		}
	}
	return comments, nil
}

func canComment(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (bool, error) {
	post, err := getPostWithAuthor(ctx, db, postID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	viewer, err := loadViewerContext(ctx, db, viewerID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !viewer.canView(post.Post) || post.WindowEndedAt != nil {
		return false, nil
	}
	if viewer.ID == post.AuthorID {
		return viewer.HasPostedToday, nil
	}
	if post.Visibility == "public" {
		return true, nil
	}
	return viewer.Friends[post.AuthorID], nil
}

// deferred 経路でセッションのリージョンが違うと関数は呼ばれず o_* が NULL になるので、ポインタで受ける
// (string のままだと走査に失敗して callWithSession の 403 に届かず 500 になる、#53)。
type interactionRow struct {
	SessRegion     *string    `db:"sess_region"`
	Result         *string    `db:"o_result"`
	CreatedAt      *time.Time `db:"o_created_at"`
	ID             *string    `db:"o_id"`
	Username       *string    `db:"o_username"`
	SecretKeyHash  *string    `db:"o_secret_key_hash"`
	DisplayName    *string    `db:"o_display_name"`
	Bio            *string    `db:"o_bio"`
	ProfileImageID *string    `db:"o_profile_image_id"`
	Region         *string    `db:"o_region"`
	AccountType    *string    `db:"o_account_type"`
	Status         *string    `db:"o_status"`
}

func (row interactionRow) err() error {
	if row.Result == nil {
		return errors.New("interaction function returned no result")
	}
	switch *row.Result {
	case "ok":
		return nil
	case "forbidden":
		return NewAPIError(http.StatusForbidden, "interaction_forbidden")
	case "not_found":
		return NewAPIError(http.StatusNotFound, "relationship_not_found")
	default:
		return fmt.Errorf("unexpected interaction result: %s", *row.Result)
	}
}

func (row interactionRow) user() (User, error) {
	user := User{
		ID: *row.ID, Username: *row.Username, SecretKeyHash: *row.SecretKeyHash, DisplayName: *row.DisplayName,
		Bio: *row.Bio, ProfileImageID: row.ProfileImageID, Region: *row.Region, AccountType: *row.AccountType, Status: *row.Status,
	}
	return user, user.setImageKey()
}

// postAccess は閲覧・コメントの可否を DB 関数で 1 往復で判定する。
func postAccess(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (canView, canComment bool, err error) {
	var row struct {
		CanView    bool `db:"can_view"`
		CanComment bool `db:"can_comment"`
	}
	err = sqlx.GetContext(ctx, db, &row, `SELECT can_view, can_comment FROM isureal_post_access($1, $2)`, viewerID, postID)
	return row.CanView, row.CanComment, err
}
