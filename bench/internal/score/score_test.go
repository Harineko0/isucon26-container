package score

import "testing"

func TestBoardMultiplierAndFloor(t *testing.T) {
	b := NewBoard()
	b.Add(FeedVerify, "asia")        // 10
	b.Add(FeedVerify, "europe")      // 15
	b.Add(FeedView, "america")       // 1.5
	if got := b.Total(); got != 26 { // 26.5 → 26
		t.Fatalf("total = %d, want 26", got)
	}
	if b.Count(FeedVerify) != 2 {
		t.Fatalf("count = %d", b.Count(FeedVerify))
	}
}

func TestWindowCounterCaps(t *testing.T) {
	c := NewWindowCounter()
	for i := 0; i < WindowCaps[ImageGet]; i++ {
		if !c.Take(ImageGet) {
			t.Fatalf("take %d should succeed", i)
		}
	}
	if c.Take(ImageGet) {
		t.Fatal("take beyond cap should fail")
	}
	if !c.Take(Export) {
		t.Fatal("uncapped kind should always succeed")
	}
	if c.Remaining(Export) != -1 || c.Remaining(ImageGet) != 0 {
		t.Fatalf("remaining = %d / %d", c.Remaining(Export), c.Remaining(ImageGet))
	}
}

func TestPointsTableMatchesManual(t *testing.T) {
	want := map[Kind]int{FeedVerify: 10, FeedVerifyFinal: 20, Export: 20, RealMojiSend: 5, CommentPost: 3, AccountDeletion: 5, MemoriesMonth: 3, MemoriesDate: 2, ImageGet: 2, UserSearch: 2, FriendRequest: 2, FriendAccept: 2, ProfileImage: 3, PastImageGet: 2}
	for k, p := range want {
		if Points[k] != p {
			t.Errorf("%s = %d, want %d", k, Points[k], p)
		}
	}
}
