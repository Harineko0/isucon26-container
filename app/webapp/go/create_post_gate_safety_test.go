package isureal

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

// このモジュールは testify を使っていない (依存を増やさない) ので、標準の比較で確かめる。
//
// create_post gate (#81, #138) を広げても安全であることの前提を確かめる:
//   1. どのリージョンでも gate は DB プールの半分以下 (投稿以外の要求の接続を残す)。
//   2. gate はちょうど設定した数だけ通し、それを超えた分は枠が空くまで待つ。
//   3. #128 以降、投稿画像の本体は gate の外でディスクに書かれ、DB には空の data が渡る
//      (gate が避けていた image_objects の TOAST の拡張ロックが起きない)。

var gateRegions = []string{"asia", "europe", "america", "other"}

func TestCreatePostGateSize_LeavesHalfThePool(t *testing.T) {
	for _, region := range gateRegions {
		gate, pool := cap(newCreatePostGate(region)), dbMaxConns(region)
		if gate <= 0 {
			t.Errorf("%s: gate size %d, want > 0", region, gate)
		}
		if gate*2 > pool {
			t.Errorf("%s: gate %d takes more than half of the DB pool %d", region, gate, pool)
		}
	}
}

// 各リージョンの実際の大きさで、ちょうど size 本まで待たずに通り、size+1 本目は release まで待つ。
func TestCreatePostGate_AdmitsExactlyItsSize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, region := range gateRegions {
			g := newCreatePostGate(region)
			size := cap(g)
			for i := range size {
				if err := g.acquire(t.Context()); err != nil {
					t.Fatalf("%s: acquire %d/%d: %v", region, i+1, size, err)
				}
			}

			admitted := make(chan error, 1)
			go func() { admitted <- g.acquire(t.Context()) }()
			synctest.Wait() // size+1 本目が枠を待って止まるまで進める
			select {
			case err := <-admitted:
				t.Fatalf("%s: acquire %d went through a full gate (err %v)", region, size+1, err)
			default:
			}

			g.release()
			if err := <-admitted; err != nil {
				t.Fatalf("%s: acquire after release: %v", region, err)
			}
			for range size {
				g.release()
			}
			if len(g) != 0 {
				t.Fatalf("%s: gate holds %d slots after releasing all, want 0", region, len(g))
			}
		}
	})
}

// 満杯の gate を待っている要求は、取り消されたら DB に行かずに戻る (枠を消費しない)。
func TestCreatePostGate_CancelledWaiterDoesNotTakeASlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newCreatePostGate("asia")
		for range cap(g) {
			if err := g.acquire(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		err := g.acquire(ctx)

		if err == nil {
			t.Fatal("acquire on a full gate returned nil after the deadline")
		}
		if len(g) != cap(g) {
			t.Fatalf("a cancelled waiter changed the gate: %d of %d", len(g), cap(g))
		}
	})
}

func TestStoreUploadedImage(t *testing.T) {
	const id = "0196f5a2-7c3d-7e4f-8a9b-0c1d2e3f4a5b"
	jpeg := bytes.Repeat([]byte{0xff, 0xd8, 0x42}, 1000)

	tests := map[string]struct {
		storeDir   func(t *testing.T) string
		wantDBData []byte
		wantRegion bool
		wantOnDisk bool
	}{
		"stored on disk: DB gets empty data and the stored region": {
			storeDir:   func(t *testing.T) string { return filepath.Join(t.TempDir(), "imgstore") },
			wantDBData: []byte{},
			wantRegion: true,
			wantOnDisk: true,
		},
		"disk unusable: falls back to keeping the bytes in the DB": {
			storeDir: func(t *testing.T) string {
				// 置き場の根が通常のファイルなので、シャードのディレクトリを作れない。
				blocker := filepath.Join(t.TempDir(), "imgstore")
				if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return blocker
			},
			wantDBData: jpeg,
			wantRegion: false,
			wantOnDisk: false,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			old := imageStoreDir
			imageStoreDir = tt.storeDir(t)
			t.Cleanup(func() { imageStoreDir = old })

			dbData, region := storeUploadedImage(id, jpeg, "asia")

			if !bytes.Equal(dbData, tt.wantDBData) || (dbData == nil) != (tt.wantDBData == nil) {
				t.Errorf("DB data: got %d bytes, want %d bytes", len(dbData), len(tt.wantDBData))
			}
			if got := region != nil; got != tt.wantRegion {
				t.Errorf("stored region set = %v, want %v", got, tt.wantRegion)
			}
			if region != nil && *region != "asia" {
				t.Errorf("stored region = %q, want asia", *region)
			}
			path, _ := imageStorePath(id)
			onDisk, err := os.ReadFile(path)
			if tt.wantOnDisk {
				if err != nil || !bytes.Equal(onDisk, jpeg) {
					t.Errorf("file on disk: err %v, %d bytes, want the uploaded %d bytes", err, len(onDisk), len(jpeg))
				}
			} else if err == nil {
				t.Errorf("file unexpectedly written to %s", path)
			}
		})
	}
}
