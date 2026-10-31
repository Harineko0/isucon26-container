package isureal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type getNotificationsResponse struct {
	ID              string    `json:"id" db:"id"`
	PostingWindowID string    `json:"postingWindowId" db:"posting_window_id"`
	Region          string    `json:"region" db:"region"`
	CreatedAt       time.Time `json:"createdAt" db:"created_at"`
}

// 通知は投稿期間の開始時にだけ作られる。プロセスに 1 本の監視を置き、このリージョンの
// 有効な投稿期間が変わったら、接続中の利用者の通知を 1 文でまとめて組み立てて各接続に配る。
// 通知は notifications 表から読まず、投稿期間と利用者から組み立てる (#27)。id は
// isureal_notification_id (004-functions.sql) で期間の開始処理が INSERT するものと同じ値になり、
// created_at は期間の starts_at と同じ値になる。対象は「その期間が有効なときにこのリージョンにいる
// active な利用者」で、開始時点でそうでなかった利用者 (退会・リージョン変更) は通知せずに切断する。
// 期間のコミットを LISTEN で検知する。起動・再接続・定期的な再同期では有効期間を DB から読み直す。
// 接続ごとの定期的な DB 照会はしない。退会・リージョン変更による切断は、接続中の利用者
// 全員を 1 秒ごとに 1 文で確認する。
//
// 取りこぼしが無いこと: 接続は、接続時の SQL (clock_timestamp() と、その時点で見えている有効な
// 投稿期間の starts_at を取る) より前に購読者として登録する。接続時に見えていなかった期間は、
// その SQL のスナップショットより後にコミットされたので、監視がその期間を見つけるのは登録の後であり、
// 購読者を集めるときに必ず含まれる。
// どの期間を配るか: 接続時に見えていなかった期間の通知だけを配る。通知の created_at はその期間の
// starts_at と同じ値で、期間の starts_at はリージョンごとに単調に増えるので、
// 「created_at > 接続時に有効だった期間の starts_at」で判定できる (有効な期間が無ければすべて配る)。
// created_at > connectedAt で判定すると、開始処理のトランザクションの途中 (starts_at の後、コミットの前) に
// 接続した利用者がその期間の通知を受け取れない (#42)。
// 状態は DB で観測した有効な期間 id だけから決まるので、どのサーバで /api/initialize が
// 実行されても期間 id の変化として検知される (プロセス内に初期化すべき状態は無い)。
const (
	notificationWindowResyncInterval = time.Second
	notificationUserCheckInterval    = time.Second
	notificationQueryTimeout         = 2 * time.Second
)

type windowNotification struct {
	UserID    string
	ID        string
	CreatedAt time.Time
	kick      bool
}

// notificationEvent は購読者に配るもの。kick なら接続を閉じる。
type notificationEvent struct {
	kick         bool
	notification getNotificationsResponse
}

type notificationSubscriber struct {
	userID string
	wake   chan struct{}

	mu      sync.Mutex
	pending []notificationEvent
}

func (s *notificationSubscriber) push(ev notificationEvent) {
	s.mu.Lock()
	s.pending = append(s.pending, ev)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *notificationSubscriber) take() []notificationEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs := s.pending
	s.pending = nil
	return evs
}

// next は after より後に作られた通知を待つ。ok が false なら通知せずに接続を閉じる。
func (s *notificationSubscriber) next(ctx context.Context, after time.Time) (getNotificationsResponse, bool) {
	for {
		for _, ev := range s.take() {
			if ev.kick {
				return getNotificationsResponse{}, false
			}
			if ev.notification.CreatedAt.After(after) {
				return ev.notification, true
			}
		}
		select {
		case <-ctx.Done():
			return getNotificationsResponse{}, false
		case <-s.wake:
		}
	}
}

type postingWindowWatcher struct {
	region string

	// DB へのアクセス (テストでは差し替える)。
	inactiveUsers func(ctx context.Context, userIDs []string, region string) ([]string, error)

	mu   sync.Mutex
	subs map[string]map[*notificationSubscriber]struct{}
	// last は配り終えた有効な投稿期間の id (無ければ "")。
	last string
}

func newPostingWindowWatcher(db *sqlx.DB, region string) *postingWindowWatcher {
	w := newPostingWindowWatcherWith(region,
		func(ctx context.Context, userIDs []string, region string) ([]string, error) {
			var ids []string
			err := db.SelectContext(ctx, &ids, `
    SELECT ids.id FROM unnest($1::text[]) AS ids(id)
    WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = ids.id AND u.status = 'active' AND u.region = $2)
   `, userIDs, region)
			return ids, err
		},
	)
	go w.runWindowLoop(context.Background(), db)
	go w.runUserCheckLoop()
	return w
}

func newPostingWindowWatcherWith(
	region string,
	inactiveUsers func(ctx context.Context, userIDs []string, region string) ([]string, error),
) *postingWindowWatcher {
	return &postingWindowWatcher{
		region:        region,
		inactiveUsers: inactiveUsers,
		subs:          map[string]map[*notificationSubscriber]struct{}{},
	}
}

func (w *postingWindowWatcher) subscribe(userID string) *notificationSubscriber {
	s := &notificationSubscriber{userID: userID, wake: make(chan struct{}, 1)}
	w.mu.Lock()
	defer w.mu.Unlock()
	set := w.subs[userID]
	if set == nil {
		set = map[*notificationSubscriber]struct{}{}
		w.subs[userID] = set
	}
	set[s] = struct{}{}
	return s
}

func (w *postingWindowWatcher) unsubscribe(s *notificationSubscriber) {
	w.mu.Lock()
	defer w.mu.Unlock()
	set := w.subs[s.userID]
	delete(set, s)
	if len(set) == 0 {
		delete(w.subs, s.userID)
	}
}

// snapshot は接続中の利用者 id と、その購読者を返す。
func (w *postingWindowWatcher) snapshot() ([]string, map[string][]*notificationSubscriber) {
	w.mu.Lock()
	defer w.mu.Unlock()
	userIDs := make([]string, 0, len(w.subs))
	targets := make(map[string][]*notificationSubscriber, len(w.subs))
	// Share a backing array across users in this snapshot. Most users have
	// one connection, and both the periodic check and window delivery take it.
	subscribers := make([]*notificationSubscriber, 0, len(w.subs))
	for userID, set := range w.subs {
		userIDs = append(userIDs, userID)
		start := len(subscribers)
		for s := range set {
			subscribers = append(subscribers, s)
		}
		end := len(subscribers)
		targets[userID] = subscribers[start:end:end]
	}
	return userIDs, targets
}

// checkWindow はコミット済みの期間を接続中の利用者へ配る。hint は NOTIFY が知らせた id。
// 再同期では先に有効期間を読み、その後で購読者を集める（接続時の照会との間に隙間を作らない）。
func (w *postingWindowWatcher) checkWindow(ctx context.Context, db windowReader, hint string) error {
	id := hint
	if id == "" {
		var err error
		id, err = db.activeWindow(ctx, w.region)
		if err != nil {
			return err
		}
	}
	w.mu.Lock()
	unchanged := id == w.last
	w.mu.Unlock()
	if unchanged {
		return nil
	}
	if id != "" {
		// 期間を見つけた後に購読者を集める (取りこぼしが無い理由は先頭のコメント)。
		userIDs, targets := w.snapshot()
		var rows []windowNotification
		if len(userIDs) > 0 {
			var err error
			rows, err = db.notifications(ctx, w.region, id, userIDs)
			if err != nil {
				return err
			}
		}
		// ヒントだけでは last を進めない。終了済みや削除済みの期間の再送にも同じ条件を使う。
		if hint != "" && len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			ev := notificationEvent{notification: getNotificationsResponse{
				ID: row.ID, PostingWindowID: id, Region: w.region, CreatedAt: row.CreatedAt,
			}}
			if row.kick {
				ev = notificationEvent{kick: true}
			}
			for _, s := range targets[row.UserID] {
				s.push(ev)
			}
		}
	}
	w.mu.Lock()
	w.last = id
	w.mu.Unlock()
	return nil
}

func (w *postingWindowWatcher) runUserCheckLoop() {
	for {
		time.Sleep(notificationUserCheckInterval)
		ctx, cancel := context.WithTimeout(context.Background(), notificationQueryTimeout)
		if err := w.checkUsers(ctx); err != nil {
			log.Printf("failed to check notification subscribers: %v", err)
		}
		cancel()
	}
}

// checkUsers は退会・リージョン変更した利用者の接続を閉じる。
func (w *postingWindowWatcher) checkUsers(ctx context.Context) error {
	userIDs, targets := w.snapshot()
	if len(userIDs) == 0 {
		return nil
	}
	gone, err := w.inactiveUsers(ctx, userIDs, w.region)
	if err != nil {
		return err
	}
	for _, userID := range gone {
		for _, s := range targets[userID] {
			s.push(notificationEvent{kick: true})
		}
	}
	return nil
}

// notificationsAfter は、接続時に有効だった投稿期間の starts_at (無ければ nil) から、
// 配る通知の created_at の下限 (これより後なら配る) を返す。理由は postingWindowWatcher のコメント。
func notificationsAfter(activeWindowStartsAt *time.Time) time.Time {
	if activeWindowStartsAt == nil {
		return time.Time{}
	}
	return *activeWindowStartsAt
}

type notificationConnectState struct {
	Found                bool       `db:"found"`
	Region               *string    `db:"region"`
	ActiveWindowStartsAt *time.Time `db:"active_window_starts_at"`
}

func getNotificationsHandler(h *HandlerContext) echo.HandlerFunc {
	connects := &notificationConnectQueue{db: h.DB, region: h.Config.Region}
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		session := GetSession(c)
		userID := session.UserID
		// 接続時の SQL より前に登録する (取りこぼしが無い理由は postingWindowWatcher のコメント)。
		sub := h.Windows.subscribe(userID)
		defer h.Windows.unsubscribe(sub)
		var start notificationConnectState
		if tokenHash, deferred := deferredSession(c); deferred {
			// セッションの確認 (401 → 403) も同じ 1 文で行い、ヘッダを送る前に判定する。
			var err error
			start, err = connects.get(ctx, tokenHash, userID)
			if err != nil {
				return err
			}
			region := ""
			if start.Region != nil {
				region = *start.Region
			}
			if err := rejectDeferredSession(c, h, tokenHash, start.Found, region); err != nil {
				return err
			}
		} else {
			// セッションは SessionMiddleware で確認済み。その後の退会・リージョン変更はここで見る。
			if err := h.DB.GetContext(ctx, &start, `
   SELECT true AS found,
          (SELECT region FROM users WHERE id = $1 AND status = 'active') AS region,
          (SELECT starts_at FROM posting_windows WHERE region = $2 AND ended_at IS NULL) AS active_window_starts_at
  `, userID, h.Config.Region); err != nil {
				return err
			}
		}
		after := notificationsAfter(start.ActiveWindowStartsAt)
		response := c.Response()
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(response)
		if err := controller.Flush(); err != nil {
			return nil
		}
		if start.Region == nil || *start.Region != session.Region {
			return nil
		}
		notification, ok := sub.next(ctx, after)
		if !ok {
			return nil
		}
		data, err := json.Marshal(notification)
		if err != nil {
			c.Logger().Error("failed to encode notification", "error", err)
			return nil
		}
		if _, err := fmt.Fprintf(response, "data: %s\n\n", data); err != nil {
			return nil
		}
		_ = controller.Flush()
		return nil
	}
}
