package isureal

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

type HandlerContext struct {
	imageReads *imageReads
	DB         *sqlx.DB
	Config     *Config
	IPBans     *IPBanList
	Windows    *postingWindowWatcher
}

func NewServer(config *Config) *echo.Echo {
	if !validRegion(config.Region) {
		panic("invalid server region: " + config.Region)
	}
	e := echo.New()
	e.IPExtractor = extractClientIP

	setInternalSecret(config)
	if err := ensureImageStoreDirs(); err != nil {
		panic("failed to prepare image store: " + err.Error())
	}
	go removeImageStoreTrash()
	startImagePushers(config.Region)

	ipBans := newIPBanList()
	e.Use(ActionLogMiddleware(config.LogLevel, os.Stdout, ipBans.observeActionLog))
	e.Use(middleware.Recover())
	e.HTTPErrorHandler = APIErrorHandler

	primary, err := openDB(config)
	if err != nil {
		panic("failed to open database: " + err.Error())
	}
	db, reads, control, err := openRequestDBs(config, primary, os.Getenv("ISUREAL_APP_DB_POOL"), os.Getenv("ISUREAL_APP_DB_READ_NAME"))
	if err != nil {
		panic("failed to open application DB pool: " + err.Error())
	}
	startRequestPool(db, "requests")
	if reads != db {
		startRequestPool(reads, "reads")
	}

	handlerContext := &HandlerContext{
		DB:      db,
		Config:  config,
		IPBans:  ipBans,
		Windows: newPostingWindowWatcher(primary, config.Region),
	}
	readContext := *handlerContext
	readContext.DB = reads
	readContext.imageReads = &imageReads{load: func(ctx context.Context, rel string, takeRequests func() []imageReadRequest) []imageReadResult {
		return loadImageReadBatch(ctx, &readContext, rel, takeRequests)
	}}
	e.Pre(InternalMiddleware(handlerContext))
	go ipBans.abuse.writeLoop(db)
	e.Use(IPBanMiddleware(handlerContext))
	go refreshSearchNorms(db)
	go cleanSearchDirtyLoop(db)
	go loadInitialDataBoundary(db)
	go loadProfileImageHints(db)
	// 再起動 (デプロイ・追試) 直後の DB のキャッシュを埋める。DB と同じリージョンの asia だけが行う
	// (3 台が同時に読まない。initialize も asia1 に来る)。prewarm.go
	if config.Region == "asia" {
		go prewarmDB(db, "startup")
	}

	registerRoutes(e, handlerContext, &readContext, control)

	return e
}

func startRequestPool(db *sqlx.DB, name string) {
	if err := db.Ping(); err != nil {
		panic("failed to ping " + name + " pool: " + err.Error())
	}
	go warmUpDBPool(db, db.Stats().MaxOpenConnections)
	go func() {
		for range time.Tick(5 * time.Second) {
			st := db.Stats()
			if st.WaitCount > 0 || st.InUse > 0 {
				fmt.Fprintf(os.Stderr, "dbstats open=%d inuse=%d idle=%d wait=%d waitdur=%s pool=%s\n", st.OpenConnections, st.InUse, st.Idle, st.WaitCount, st.WaitDuration, name)
			}
		}
	}()
}

// registerRoutes は API のルートを登録する。テスト (roundtrip_routes_test.go) も同じ表を使う。
func registerRoutes(e *echo.Echo, h, reads *HandlerContext, control *sqlx.DB) {
	e.POST("/api/initialize", initializeHandler(h))
	e.POST("/api/auth/register", registerHandler(h))
	e.POST("/api/auth/login", loginHandler(h))
	e.POST("/api/auth/logout", logoutHandler(h), SessionMiddleware(h, true))
	e.GET("/api/users", searchUsersHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.GET("/api/users/:userId", getUserHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.GET("/api/me/export", exportPostsHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.GET("/api/me", getMeHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.PATCH("/api/me", updateMeHandler(h), SessionMiddleware(h, true))
	e.PUT("/api/me/profile-image", putProfileImageHandler(h), SessionMiddleware(h, true))
	e.DELETE("/api/me/profile-image", deleteProfileImageHandler(h), SessionMiddleware(h, true))
	e.POST("/api/me/account-deletion", deleteAccountHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.POST("/api/posts", createPostHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.GET("/api/posting-window/current", getCurrentPostingWindowHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.GET("/api/posts/me/current", getMyCurrentPostHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.GET("/api/posts/:postId", getPostHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.PATCH("/api/posts/:postId", updatePostHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.DELETE("/api/posts/:postId", deletePostHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.GET("/api/feeds/:feed", getFeedHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.GET("/api/memories", getMemoriesHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.GET("/api/memories/:date", getMemoriesByDateHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.POST("/api/posts/:postId/comments", createCommentHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.GET("/api/posts/:postId/comments", getCommentsHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.DELETE("/api/comments/:commentId", deleteCommentHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.POST("/api/posts/:postId/realmojis", replaceRealMojiHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.GET("/api/posts/:postId/realmojis", getRealMojisHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.DELETE("/api/realmojis/:realMojiId", deleteRealMojiHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.GET("/api/images/:imageId", getImageHandler(reads))
	e.GET("/api/notifications", getNotificationsHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.POST("/api/friend-requests", createFriendRequestHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.POST("/api/friend-requests/:requestId/:action", transitionFriendRequestHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.GET("/api/relationships/:kind", getRelationshipsHandler(reads), SessionMiddleware(reads, true), RegionMiddleware(reads))
	e.DELETE("/api/friendships/:userId", deleteFriendshipHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.PUT("/api/follows/:userId", followHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.DELETE("/api/follows/:userId", unfollowHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.PUT("/api/blocks/:userId", blockHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.DELETE("/api/blocks/:userId", unblockHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.POST("/api/admin/auth/login", adminLoginHandler(h))
	e.POST("/api/admin/auth/logout", adminLogoutHandler(h), AdminSessionMiddleware(h, true))
	e.PUT("/api/admin/users/:userId/certification", certifyUserHandler(h), AdminSessionMiddleware(h, true))
	e.DELETE("/api/admin/users/:userId/certification", deleteUserCertificationHandler(h), AdminSessionMiddleware(h, true))
	registerAdminPostingWindow(e, control, h.DB)
}
