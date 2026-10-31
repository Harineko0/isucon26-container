package isureal

import (
	"strings"
	"testing"
)

func testCachedUser(t *testing.T, id, username, displayName, status string) *cachedUser {
	t.Helper()
	cu, err := newCachedUser(User{ID: id, Username: username, DisplayName: displayName, Status: status, SecretKeyHash: strings.Repeat("ab", 32), AccountType: "regular"})
	if err != nil {
		t.Fatal(err)
	}
	return cu
}

func TestUsersCacheSearch(t *testing.T) {
	c := &usersCache{users: map[string]*cachedUser{}}
	for _, u := range []*cachedUser{
		testCachedUser(t, "u3", "carol", "Carol", "active"),
		testCachedUser(t, "u1", "alice", "ALICE Smith", "active"),
		testCachedUser(t, "u2", "bob", "Bob", "active"),
		testCachedUser(t, "u5", "alice_2", "Älice", "active"),
		testCachedUser(t, "u4", "al", "gone", "deleted"),
		testCachedUser(t, "u6", "alice", "same name other id", "active"),
	} {
		c.users[u.ID] = u
		c.insertSorted(u)
	}
	got := func(users []User) string {
		ids := make([]string, len(users))
		for i, u := range users {
			ids[i] = u.ID
		}
		return strings.Join(ids, ",")
	}
	// (username, id) 順。deleted は出ない。本人は出ない。
	if s := got(c.search("u2", normalizeSearchText("ALI"), "", "", nil, 51)); s != "u1,u6,u5" {
		t.Fatalf("search: %s", s)
	}
	if s := got(c.search("u1", normalizeSearchText("ali"), "", "", nil, 51)); s != "u6,u5" {
		t.Fatalf("search excluding viewer: %s", s)
	}
	// ブロック関係は双方向で除く。
	if s := got(c.search("u2", "ali", "", "", map[string]bool{"u6": true}, 51)); s != "u1,u5" {
		t.Fatalf("search with blocked: %s", s)
	}
	// ページング: (username, id) > (before)。
	if s := got(c.search("u2", "ali", "alice", "u1", nil, 51)); s != "u6,u5" {
		t.Fatalf("paginated: %s", s)
	}
	if s := got(c.search("u2", "ali", "alice", "u6", nil, 51)); s != "u5" {
		t.Fatalf("paginated 2: %s", s)
	}
	// limit
	if s := got(c.search("u2", "ali", "", "", nil, 1)); s != "u1" {
		t.Fatalf("limit: %s", s)
	}
	// 表示名の変更 (削除して入れ直す)。
	old := c.users["u2"]
	c.removeSorted(old)
	delete(c.users, "u2")
	nu := testCachedUser(t, "u2", "bob", "Alicia", "active")
	c.users["u2"] = nu
	c.insertSorted(nu)
	if s := got(c.search("u3", "ali", "", "", nil, 51)); s != "u1,u6,u5,u2" {
		t.Fatalf("after rename: %s", s)
	}
	if len(c.sorted) != 6 {
		t.Fatalf("sorted length: %d", len(c.sorted))
	}
}
