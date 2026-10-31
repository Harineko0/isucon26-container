package isureal

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

type fakeNotificationDB struct {
	mu       sync.Mutex
	window   string
	rows     map[string][]windowNotification // windowID -> rows
	fetchErr error
	fetches  [][]string
	inactive []string
}

func (f *fakeNotificationDB) watcher() *postingWindowWatcher {
	return newPostingWindowWatcherWith("asia",
		func(ctx context.Context, region string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.window, nil
		},
		func(ctx context.Context, windowID string, userIDs []string) ([]windowNotification, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.fetchErr != nil {
				return nil, f.fetchErr
			}
			ids := append([]string(nil), userIDs...)
			sort.Strings(ids)
			f.fetches = append(f.fetches, ids)
			want := map[string]bool{}
			for _, id := range userIDs {
				want[id] = true
			}
			var out []windowNotification
			for _, r := range f.rows[windowID] {
				if want[r.UserID] {
					out = append(out, r)
				}
			}
			return out, nil
		},
		func(ctx context.Context, userIDs []string, region string) ([]string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.inactive, nil
		},
	)
}

func waitNext(t *testing.T, s *notificationSubscriber, connectedAt time.Time) (getNotificationsResponse, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	return s.next(ctx, connectedAt)
}

func TestWatcherDeliversNewWindowWithOneFetch(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	f := &fakeNotificationDB{window: "w0", rows: map[string][]windowNotification{
		"w0": {{UserID: "u1", ID: "old", CreatedAt: base, UserActive: true, UserRegion: "asia"}},
		"w1": {
			{UserID: "u1", ID: "n1", CreatedAt: base.Add(time.Minute), UserActive: true, UserRegion: "asia"},
			{UserID: "u2", ID: "n2", CreatedAt: base.Add(time.Minute), UserActive: true, UserRegion: "asia"},
			{UserID: "u3", ID: "n3", CreatedAt: base.Add(time.Minute), UserActive: false, UserRegion: "asia"},
		},
	}}
	w := f.watcher()
	ctx := context.Background()
	if err := w.checkWindow(ctx); err != nil { // w0 seen with no subscribers: no fetch
		t.Fatal(err)
	}
	connectedAt := base.Add(time.Second)
	s1a, s1b, s2, s3 := w.subscribe("u1"), w.subscribe("u1"), w.subscribe("u2"), w.subscribe("u3")
	if err := w.checkWindow(ctx); err != nil { // unchanged
		t.Fatal(err)
	}
	if len(f.fetches) != 0 {
		t.Fatalf("unexpected fetches: %v", f.fetches)
	}
	f.mu.Lock()
	f.window = "w1"
	f.mu.Unlock()
	if err := w.checkWindow(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.fetches) != 1 || len(f.fetches[0]) != 3 {
		t.Fatalf("want one fetch for 3 users, got %v", f.fetches)
	}
	for _, s := range []*notificationSubscriber{s1a, s1b} {
		n, ok := waitNext(t, s, connectedAt)
		if !ok || n.ID != "n1" || n.PostingWindowID != "w1" || n.Region != "asia" {
			t.Fatalf("u1: got %+v %v", n, ok)
		}
	}
	if n, ok := waitNext(t, s2, connectedAt); !ok || n.ID != "n2" {
		t.Fatalf("u2: got %+v %v", n, ok)
	}
	if _, ok := waitNext(t, s3, connectedAt); ok {
		t.Fatal("inactive user must be disconnected without a notification")
	}
}

func TestWatcherSkipsNotificationsNotAfterConnectedAt(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	f := &fakeNotificationDB{rows: map[string][]windowNotification{
		"w1": {{UserID: "u1", ID: "n1", CreatedAt: base, UserActive: true, UserRegion: "asia"}},
	}}
	w := f.watcher()
	s := w.subscribe("u1")
	f.window = "w1"
	if err := w.checkWindow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := waitNext(t, s, base); ok { // created_at == connectedAt: not delivered
		t.Fatal("notification not after connectedAt must not be delivered")
	}
}

func TestWatcherRetriesAfterFetchError(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	f := &fakeNotificationDB{window: "w1", fetchErr: errors.New("boom"), rows: map[string][]windowNotification{
		"w1": {{UserID: "u1", ID: "n1", CreatedAt: base.Add(time.Minute), UserActive: true, UserRegion: "asia"}},
	}}
	w := f.watcher()
	s := w.subscribe("u1")
	if err := w.checkWindow(context.Background()); err == nil {
		t.Fatal("want error")
	}
	f.mu.Lock()
	f.fetchErr = nil
	f.mu.Unlock()
	if err := w.checkWindow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, ok := waitNext(t, s, base); !ok || n.ID != "n1" {
		t.Fatalf("got %+v %v", n, ok)
	}
}

// /api/initialize が別のサーバで実行されると、有効な期間が消えたり初期データの期間に戻ったりする。
// その後に新しい期間が始まれば配られる。
func TestWatcherFollowsInitializeOnAnotherServer(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	f := &fakeNotificationDB{window: "bench", rows: map[string][]windowNotification{
		"init": {{UserID: "u1", ID: "old", CreatedAt: base, UserActive: true, UserRegion: "asia"}},
		"new":  {{UserID: "u1", ID: "n", CreatedAt: base.Add(time.Hour), UserActive: true, UserRegion: "asia"}},
	}}
	w := f.watcher()
	ctx := context.Background()
	_ = w.checkWindow(ctx)
	s := w.subscribe("u1")
	connectedAt := base.Add(time.Minute)
	for _, id := range []string{"", "init", "new"} {
		f.mu.Lock()
		f.window = id
		f.mu.Unlock()
		if err := w.checkWindow(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n, ok := waitNext(t, s, connectedAt); !ok || n.ID != "n" {
		t.Fatalf("got %+v %v", n, ok)
	}
}

func TestWatcherCheckUsersKicksAndUnsubscribe(t *testing.T) {
	f := &fakeNotificationDB{inactive: []string{"u1"}}
	w := f.watcher()
	s1, s2 := w.subscribe("u1"), w.subscribe("u2")
	if err := w.checkUsers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := waitNext(t, s1, time.Time{}); ok {
		t.Fatal("u1 must be kicked")
	}
	w.unsubscribe(s1)
	w.unsubscribe(s2)
	if ids, _ := w.snapshot(); len(ids) != 0 {
		t.Fatalf("subscribers left: %v", ids)
	}
}

func TestWatcherConcurrentSubscribers(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	f := &fakeNotificationDB{rows: map[string][]windowNotification{}}
	var rows []windowNotification
	users := make([]string, 200)
	for i := range users {
		users[i] = "u" + string(rune('a'+i%26)) + time.Duration(i).String()
		rows = append(rows, windowNotification{UserID: users[i], ID: "n" + users[i], CreatedAt: base.Add(time.Minute), UserActive: true, UserRegion: "asia"})
	}
	f.rows["w1"] = rows
	w := f.watcher()
	subs := make([]*notificationSubscriber, len(users))
	for i, u := range users {
		subs[i] = w.subscribe(u)
	}
	var wg sync.WaitGroup
	got := make([]bool, len(users))
	for i := range subs {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			n, ok := subs[i].next(ctx, base)
			got[i] = ok && n.ID == "n"+users[i]
		})
	}
	f.mu.Lock()
	f.window = "w1"
	f.mu.Unlock()
	if err := w.checkWindow(context.Background()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i, ok := range got {
		if !ok {
			t.Fatalf("subscriber %d missed its notification", i)
		}
	}
	if len(f.fetches) != 1 {
		t.Fatalf("want 1 fetch, got %d", len(f.fetches))
	}
}
