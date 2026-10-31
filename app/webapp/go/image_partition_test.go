package isureal

import (
	"testing"
	"time"
)

func TestImagePartitionFor(t *testing.T) {
	cases := []struct {
		at   time.Time
		want string
	}{
		{time.Date(2024, 8, 31, 23, 59, 59, 0, time.UTC), "image_objects_default"},
		{time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC), "image_objects_202409"},
		{time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC), "image_objects_202512"},
		{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), "image_objects_202610"},
		{time.Date(2026, 10, 28, 5, 59, 59, 0, time.UTC), "image_objects_202610"},
		{time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC), "image_objects_20261028"},
		{time.Date(2026, 10, 31, 15, 0, 0, 0, time.FixedZone("JST", 9*3600)), "image_objects_20261028"},
		{time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "image_objects_202611"},
		{time.Date(2027, 6, 30, 23, 59, 59, 0, time.UTC), "image_objects_202706"},
		{time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC), "image_objects_default"},
	}
	for _, c := range cases {
		if got := imagePartitionFor(c.at); got != c.want {
			t.Errorf("imagePartitionFor(%s) = %s, want %s", c.at, got, c.want)
		}
	}
}

func TestRememberImagePartition(t *testing.T) {
	now := time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC)
	rememberImagePartition("a", "image_objects_20261028_p3")
	rememberImagePartition("b", "image_objects_20261028")
	rememberImagePartition("c", "image_objects")
	rememberImagePartition("d", "public.image_objects_202409")
	if rel, exact := imageRelationFor("a", now); rel != "image_objects_20261028_p3" || !exact {
		t.Errorf("a: %s %v", rel, exact)
	}
	// 手がかりが無ければ今のパーティションを推測する (外れたら親表で引き直す)。
	for _, id := range []string{"b", "c", "d"} {
		if rel, exact := imageRelationFor(id, now); rel != "image_objects_20261028" || exact {
			t.Errorf("%s: %s %v", id, rel, exact)
		}
	}
	if rel, exact := imageRelationFor("b", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)); rel != "image_objects_202611" || exact {
		t.Errorf("b in November: %s %v", rel, exact)
	}
	imageCreatedAt.Store("e", time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
	if rel, exact := imageRelationFor("e", now); rel != "image_objects_202610" || exact {
		t.Errorf("e: %s %v", rel, exact)
	}
}
