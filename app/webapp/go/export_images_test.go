package isureal

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCollectExportImagesMixesLocalAndRemoteRows(t *testing.T) {
	asia := "asia"
	var rows []exportImageRow
	want := map[string]string{}
	// 外部保存とDB保存を交互に並べ、goroutine の書き込みと同期の書き込みが重なる並びにする。
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("img-%03d", i)
		if i%2 == 0 {
			rows = append(rows, exportImageRow{ID: id, StoredRegion: &asia, CreatedAt: time.Unix(int64(i), 0)})
			want[id] = "remote:" + id
		} else {
			rows = append(rows, exportImageRow{ID: id, Data: []byte("local:" + id), CreatedAt: time.Unix(int64(i), 0)})
			want[id] = "local:" + id
		}
	}
	images := map[string][]byte{}
	var local int
	err := collectExportImages(rows, images, func(id, region string) ([]byte, error) {
		if region != asia {
			t.Errorf("region = %q, want %q", region, asia)
		}
		time.Sleep(time.Millisecond)
		return []byte("remote:" + id), nil
	}, func(exportImageRow) { local++ })
	if err != nil {
		t.Fatalf("collectExportImages: %v", err)
	}
	if local != 100 {
		t.Errorf("onLocal calls = %d, want 100", local)
	}
	if len(images) != len(want) {
		t.Fatalf("len(images) = %d, want %d", len(images), len(want))
	}
	for id, w := range want {
		if got := string(images[id]); got != w {
			t.Errorf("images[%s] = %q, want %q", id, got, w)
		}
	}
}

func TestCollectExportImagesReturnsFetchError(t *testing.T) {
	europe := "europe"
	rows := []exportImageRow{{ID: "a", StoredRegion: &europe}, {ID: "b", Data: []byte("x")}}
	boom := errors.New("boom")
	err := collectExportImages(rows, map[string][]byte{}, func(string, string) ([]byte, error) { return nil, boom }, func(exportImageRow) {})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
