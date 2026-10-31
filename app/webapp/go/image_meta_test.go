package isureal

import "testing"

func TestOwnerLookupVerdict(t *testing.T) {
	tests := []struct {
		name        string
		found       bool
		signatureOK bool
		allowed     bool
		want        imageVerdict
	}{
		// 所有者が active でない (または居ない): 従来も行が無く、署名に関係なくフラグ無しの 403。
		{name: "owner inactive, valid signature", found: false, signatureOK: true, allowed: false, want: imageForbidden},
		{name: "owner inactive, invalid signature", found: false, signatureOK: false, allowed: false, want: imageForbidden},
		{name: "allowed and valid signature", found: true, signatureOK: true, allowed: true, want: imageServe},
		// 削除済み投稿の画像など: 従来は行があれば署名 OK で 403、行が無くても 403 (どちらもフラグ無し)。
		{name: "not allowed, valid signature", found: true, signatureOK: true, allowed: false, want: imageForbidden},
		// allowed なら画像の行はあるので、従来と同じくフラグ付き。
		{name: "allowed, invalid signature", found: true, signatureOK: false, allowed: true, want: imageInvalidSignature},
		// 行の有無でフラグが変わるので従来の検索に任せる。
		{name: "not allowed, invalid signature", found: true, signatureOK: false, allowed: false, want: imageFullLookup},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownerLookupVerdict(tt.found, tt.signatureOK, tt.allowed); got != tt.want {
				t.Errorf("ownerLookupVerdict(%v, %v, %v) = %v, want %v", tt.found, tt.signatureOK, tt.allowed, got, tt.want)
			}
		})
	}
}

func TestLoadImageMeta(t *testing.T) {
	imageMetas.Clear()
	t.Cleanup(imageMetas.Clear)
	if _, ok := loadImageMeta("img-1"); ok {
		t.Fatal("loadImageMeta on empty cache: got ok, want miss")
	}
	want := imageMeta{ownerID: "user-1", mediaType: "image/jpeg"}
	imageMetas.Store("img-1", want)
	got, ok := loadImageMeta("img-1")
	if !ok || got != want {
		t.Fatalf("loadImageMeta = %+v, %v; want %+v, true", got, ok, want)
	}
	imageMetas.Clear()
	if _, ok := loadImageMeta("img-1"); ok {
		t.Fatal("loadImageMeta after Clear: got ok, want miss")
	}
}
