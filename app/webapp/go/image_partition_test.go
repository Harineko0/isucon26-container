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

func TestImageRelationFor_TimeID(t *testing.T) {
	// 手がかりが無くても、時刻先頭の id (他のサーバー経由で上げた画像) なら中間パーティションを引く。
	id := timeIDAt(time.Date(2026, 10, 31, 0, 30, 0, 0, time.UTC), "f2")
	if got := imageRelationFor(id); got != "image_objects_20261028" {
		t.Errorf("no hint: %s", got)
	}
	// DB から教わった葉があればそちらが優先。
	rememberImagePartition(id, "image_objects_20261028_p5")
	if got := imageRelationFor(id); got != "image_objects_20261028_p5" {
		t.Errorf("leaf hint: %s", got)
	}
	// 乱数 UUID (初期データ) は従来どおり手がかり無し。
	if got := imageRelationFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301"); got != "" {
		t.Errorf("random uuid: %s", got)
	}
}
