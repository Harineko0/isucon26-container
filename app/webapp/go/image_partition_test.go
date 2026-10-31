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
	rememberImagePartition("a", "image_objects_20261028_p3")
	rememberImagePartition("b", "image_objects_20261028")
	rememberImagePartition("c", "image_objects")
	rememberImagePartition("d", "public.image_objects_202409")
	if got := imageRelationFor("a"); got != "image_objects_20261028_p3" {
		t.Errorf("a: %s", got)
	}
	for _, id := range []string{"b", "c", "d"} {
		if got := imageRelationFor(id); got != "" {
			t.Errorf("%s: %s", id, got)
		}
	}
	imageCreatedAt.Store("e", time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	if got := imageRelationFor("e"); got != "image_objects_20261028" {
		t.Errorf("e: %s", got)
	}
}
