package isureal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

type PostLocation struct {
	Kind      string   `json:"kind" db:"location_kind"`
	Latitude  *float64 `json:"latitude,omitempty" db:"latitude"`
	Longitude *float64 `json:"longitude,omitempty" db:"longitude"`
	Locality  *string  `json:"locality,omitempty" db:"locality"`
}

func (location PostLocation) valid() bool {
	switch location.Kind {
	case "none":
		return location.Latitude == nil && location.Longitude == nil && location.Locality == nil
	case "exact":
		return location.Latitude != nil && location.Longitude != nil && location.Locality == nil &&
			*location.Latitude >= -90 && *location.Latitude <= 90 && *location.Longitude >= -180 && *location.Longitude <= 180
	case "approximate":
		return location.Latitude == nil && location.Longitude == nil && location.Locality != nil && validUserText(*location.Locality, 1, 100)
	default:
		return false
	}
}

type Post struct {
	ID              string `db:"id"`
	AuthorID        string `db:"author_id"`
	PostingWindowID string `db:"posting_window_id"`
	ImageID         string `db:"image_id"`
	Caption         string `db:"caption"`
	Visibility      string `db:"visibility"`
	PostLocation
	PostedAt       time.Time  `db:"posted_at"`
	OnTimeDeadline time.Time  `db:"on_time_deadline"`
	WindowEndedAt  *time.Time `db:"window_ended_at"`
}

const postColumns = `p.id, p.author_id, p.posting_window_id, p.image_id, p.caption, p.visibility,
  p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at,
  w.on_time_deadline, w.ended_at AS window_ended_at`

func getPost(ctx context.Context, db sqlx.QueryerContext, postID string) (Post, error) {
	var post Post
	err := sqlx.GetContext(ctx, db, &post, `
  SELECT `+postColumns+`
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.id = $1 AND p.deleted_at IS NULL
 `, postID)
	return post, err
}

type PostWithAuthor struct {
	Post
	Author User `db:"author"`
}

// postWithAuthorColumnsCoalesced は LEFT JOIN LATERAL の番兵の行 (全列 NULL) を PostWithAuthor にそのまま読めるよう、
// NOT NULL の列を空文字や epoch で埋めた選択リストを返す。
func postWithAuthorColumnsCoalesced(alias string) string {
	return fmt.Sprintf(`COALESCE(%[1]s.id, '') AS id, COALESCE(%[1]s.author_id, '') AS author_id,
		COALESCE(%[1]s.posting_window_id, '') AS posting_window_id, COALESCE(%[1]s.image_id, '') AS image_id,
		COALESCE(%[1]s.caption, '') AS caption, COALESCE(%[1]s.visibility, '') AS visibility,
		COALESCE(%[1]s.location_kind, '') AS location_kind, %[1]s.latitude, %[1]s.longitude, %[1]s.locality,
		COALESCE(%[1]s.posted_at, 'epoch') AS posted_at, COALESCE(%[1]s.on_time_deadline, 'epoch') AS on_time_deadline,
		%[1]s.window_ended_at, `, alias) + userColumnsCoalesced(alias, "author")
}

const postsWithWindowFrom = `posts p JOIN posting_windows w ON w.id = p.posting_window_id`

func selectPostsWithAuthor(ctx context.Context, db sqlx.QueryerContext, conditions string, args ...any) ([]PostWithAuthor, error) {
	return selectPostsWithAuthorFrom(ctx, db, postsWithWindowFrom, conditions, args...)
}

// selectPostsWithAuthorFrom は FROM 句 (別名 p と w を定義すること) を差し替えられる版。
func selectPostsWithAuthorFrom(ctx context.Context, db sqlx.QueryerContext, from, conditions string, args ...any) ([]PostWithAuthor, error) {
	var posts []PostWithAuthor
	if err := sqlx.SelectContext(ctx, db, &posts, `
  SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
  FROM `+from+`
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.deleted_at IS NULL `+conditions, args...); err != nil {
		return nil, err
	}
	for i := range posts {
		if err := posts[i].Author.setImageKey(); err != nil {
			return nil, err
		}
	}
	return posts, nil
}

func getPostWithAuthor(ctx context.Context, db sqlx.QueryerContext, postID string) (PostWithAuthor, error) {
	posts, err := selectPostsWithAuthor(ctx, db, `AND p.id = $1`, postID)
	if err != nil {
		return PostWithAuthor{}, err
	}
	if len(posts) == 0 {
		return PostWithAuthor{}, sql.ErrNoRows
	}
	return posts[0], nil
}

type viewerContext struct {
	User
	HasPostedToday   bool
	Friends          map[string]bool
	FriendsOfFriends map[string]bool
	MutualFriends    map[string]int
	Blocked          map[string]bool
}

func loadViewerContext(ctx context.Context, db sqlx.QueryerContext, viewerID string) (viewerContext, error) {
	viewer := viewerContext{Friends: map[string]bool{}, FriendsOfFriends: map[string]bool{}, MutualFriends: map[string]int{}, Blocked: map[string]bool{}}
	// 利用者・当日の投稿有無・ブロック・友達・友達の友達を 1 往復で取る。
	var row struct {
		User
		HasPostedToday bool    `db:"has_posted_today"`
		Blocked        *string `db:"blocked"`
		Friends        *string `db:"friends"`
		Pairs          *string `db:"pairs"`
	}
	err := sqlx.GetContext(ctx, db, &row, `
  WITH f AS (
    SELECT CASE WHEN user_low_id = $1 THEN user_high_id ELSE user_low_id END AS id
    FROM friendships WHERE user_low_id = $1 OR user_high_id = $1
  )
  SELECT `+strings.Join(userColumns, ", ")+`,
    EXISTS(
      SELECT 1 FROM posts p
      JOIN posting_windows posted ON posted.id = p.posting_window_id
      JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                  AND current.local_date = posted.local_date
      WHERE p.author_id = u.id AND p.deleted_at IS NULL
    ) AS has_posted_today,
    (SELECT string_agg(CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END, ',')
     FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1) AS blocked,
    (SELECT string_agg(id, ',') FROM f) AS friends,
    (SELECT string_agg(pair, ',') FROM (
       SELECT fs.user_low_id || ':' || fs.user_high_id AS pair FROM friendships fs JOIN f ON fs.user_low_id = f.id
       UNION
       SELECT fs.user_low_id || ':' || fs.user_high_id AS pair FROM friendships fs JOIN f ON fs.user_high_id = f.id
     ) pairs) AS pairs
  FROM users u WHERE u.id = $1 AND u.status = 'active'
 `, viewerID)
	if err != nil {
		return viewer, err
	}
	viewer.User = row.User
	if err := viewer.User.setImageKey(); err != nil {
		return viewer, err
	}
	viewer.HasPostedToday = row.HasPostedToday
	for _, id := range splitIDs(row.Blocked) {
		viewer.Blocked[id] = true
	}
	for _, id := range splitIDs(row.Friends) {
		viewer.Friends[id] = true
	}
	for _, pair := range splitIDs(row.Pairs) {
		low, high, _ := strings.Cut(pair, ":")
		if viewer.Friends[low] {
			viewer.FriendsOfFriends[high] = true
			if high != viewer.ID {
				viewer.MutualFriends[high]++
			}
		}
		if viewer.Friends[high] {
			viewer.FriendsOfFriends[low] = true
			if low != viewer.ID {
				viewer.MutualFriends[low]++
			}
		}
	}
	return viewer, nil
}

func splitIDs(value *string) []string {
	if value == nil || *value == "" {
		return nil
	}
	return strings.Split(*value, ",")
}

func (viewer viewerContext) canView(post Post) bool {
	if post.AuthorID == viewer.ID {
		return true
	}
	if post.WindowEndedAt != nil || !viewer.HasPostedToday || viewer.Blocked[post.AuthorID] {
		return false
	}
	switch post.Visibility {
	case "public":
		return true
	case "friends":
		return viewer.Friends[post.AuthorID]
	case "friends_of_friends":
		return viewer.Friends[post.AuthorID] || viewer.FriendsOfFriends[post.AuthorID]
	default:
		return false
	}
}

func canViewPost(ctx context.Context, db sqlx.QueryerContext, viewerID, postID string) (bool, error) {
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
	return viewer.canView(post.Post), nil
}

func getFriendIDs(ctx context.Context, db sqlx.QueryerContext, userID string) ([]string, error) {
	var friendships []struct {
		UserLowID  string `db:"user_low_id"`
		UserHighID string `db:"user_high_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &friendships, `SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id = $1 OR user_high_id = $1`, userID); err != nil {
		return nil, err
	}
	var ids []string
	for _, friendship := range friendships {
		if friendship.UserLowID == userID {
			ids = append(ids, friendship.UserHighID)
		} else {
			ids = append(ids, friendship.UserLowID)
		}
	}
	return ids, nil
}

func getBlockedUserIDs(ctx context.Context, db sqlx.QueryerContext, userID string) (map[string]bool, error) {
	var ids []string
	if err := sqlx.SelectContext(ctx, db, &ids, `
  SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END
  FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
 `, userID); err != nil {
		return nil, err
	}
	blocked := make(map[string]bool, len(ids))
	for _, id := range ids {
		blocked[id] = true
	}
	return blocked, nil
}
