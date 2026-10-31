package isureal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// このモジュールは testify を使っていない (依存を増やさない) ので、標準の比較で確かめる。

// referenceImageSignature は参照実装 (webapp/rust・nodejs) と同じ式: "{exp}:{imageID}" に
// HMAC-SHA256 を 1000 回かける。最適化した実装 (キャッシュ・まとめて計算) と独立に結果を確かめる。
func referenceImageSignature(key []byte, imageID string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	for range 1000 {
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		value = mac.Sum(nil)
	}
	return value
}

// exp は「今 + 24 時間」以上 (有効期間が短くならない) で、それを超えるのは bucket 未満だけ。
// bucket の境界の前後と途中の時刻で確かめる (synctest の時計は秒の境界から始まる)。
func TestCurrentImageExp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		offsets := []time.Duration{
			0,
			1 * time.Second,
			500 * time.Millisecond,
			time.Duration(imageURLExpBucket-1) * time.Second,
			time.Duration(imageURLExpBucket) * time.Second,
			time.Duration(imageURLExpBucket)*time.Second + 999*time.Millisecond,
			37 * time.Minute,
		}
		start := time.Now()
		for _, off := range offsets {
			time.Sleep(start.Add(off).Sub(time.Now()))
			now := time.Now()
			exp := currentImageExp()
			min := now.Add(imageURLLifetime).Unix()

			if exp < min {
				t.Errorf("offset %v: exp %d is before now+24h (%d): URL would expire early", off, exp, min)
			}
			if exp-min >= imageURLExpBucket {
				t.Errorf("offset %v: exp %d is %ds past now+24h, want < %d", off, exp, exp-min, imageURLExpBucket)
			}
			if exp%imageURLExpBucket != 0 {
				t.Errorf("offset %v: exp %d is not a multiple of %d", off, exp, imageURLExpBucket)
			}
		}
	})
}

// 同じ bucket の間は exp が変わらない (署名を使い回せる) ことと、bucket が変わると exp も変わること。
func TestCurrentImageExp_StableWithinBucket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// exp は切り上げなので、同じ値になるのは「境界の 1 秒後」から「次の境界」までの bucket 秒。
		// 境界の 1 秒後にそろえてから測る (24 時間は bucket の倍数なので境界は Unix 時刻と同じ)。
		now := time.Now()
		next := time.Unix((now.Unix()/imageURLExpBucket+1)*imageURLExpBucket+1, 0)
		time.Sleep(next.Sub(now) + time.Millisecond)

		first := currentImageExp()
		time.Sleep(time.Duration(imageURLExpBucket-1) * time.Second)
		if got := currentImageExp(); got != first {
			t.Errorf("exp changed within one bucket: %d -> %d", first, got)
		}
		time.Sleep(time.Second)
		if got := currentImageExp(); got != first+imageURLExpBucket {
			t.Errorf("exp after the bucket = %d, want %d", got, first+imageURLExpBucket)
		}
	})
}

// signedImageURL が作る URL は、画像取得 (image_api.go) と同じ検査をすべて通り、署名は参照実装の式と一致する。
func TestSignedImageURL_PassesHandlerChecks(t *testing.T) {
	tests := map[string]struct {
		imageID string
	}{
		"uuid id":         {imageID: "0f0e3a4c-6b1d-4c5e-9a2b-1c2d3e4f5a6b"},
		"time-ordered id": {imageID: "0196f5a2-7c3d-7e4f-8a9b-0c1d2e3f4a5b"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			user := User{ImageKey: bytes.Repeat([]byte{7}, sha256.Size)}

			raw := signedImageURL(user, tt.imageID)

			path, rawQuery, ok := strings.Cut(raw, "?")
			if !ok || path != "/api/images/"+url.PathEscape(tt.imageID) {
				t.Fatalf("unexpected URL %q", raw)
			}
			query, err := url.ParseQuery(rawQuery)
			if err != nil || len(query["exp"]) != 1 || len(query["sig"]) != 1 {
				t.Fatalf("query %q: want exactly one exp and one sig (err %v)", rawQuery, err)
			}
			expText, sigText := query.Get("exp"), query.Get("sig")
			exp, err := strconv.ParseInt(expText, 10, 64)
			if err != nil || strconv.FormatInt(exp, 10) != expText || exp <= time.Now().Unix() {
				t.Fatalf("exp %q is rejected by the handler checks", expText)
			}
			sig, err := base64.RawURLEncoding.Strict().DecodeString(sigText)
			if err != nil || len(sig) != sha256.Size || base64.RawURLEncoding.EncodeToString(sig) != sigText {
				t.Fatalf("sig %q is rejected by the handler checks", sigText)
			}
			if !hmac.Equal(sig, imageSignature(user.ImageKey, tt.imageID, exp)) {
				t.Errorf("sig does not verify with imageSignature")
			}
			if !hmac.Equal(sig, referenceImageSignature(user.ImageKey, tt.imageID, exp)) {
				t.Errorf("sig differs from the reference implementation's formula")
			}
		})
	}
}

// まとめて計算する経路 (signaturePlan → computeImageSignatures、SIMD を使う CPU ではその実装) が、
// 参照実装の式と同じ署名をキャッシュに入れる。bucket の倍数の exp と、そうでない exp (他のサーバーや
// 以前の版が出した URL の検証) の両方で確かめる。
func TestComputeImageSignatures_MatchesReference(t *testing.T) {
	base := time.Now().Add(imageURLLifetime).Unix()
	aligned := (base/imageURLExpBucket + 1) * imageURLExpBucket
	tests := map[string]struct {
		exp int64
	}{
		"bucket-aligned exp":          {exp: aligned},
		"next bucket":                 {exp: aligned + imageURLExpBucket},
		"unaligned exp (old 10s URL)": {exp: aligned + 10},
		"unaligned exp (odd second)":  {exp: aligned + 7},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			keys := [][]byte{bytes.Repeat([]byte{1}, sha256.Size), bytes.Repeat([]byte{2}, sha256.Size)}
			ids := []string{"img-" + name + "-a", "img-" + name + "-b", "img-" + name + "-c"}
			var requests []signatureRequest
			for _, key := range keys {
				for _, id := range ids {
					requests = append(requests, signatureRequest{key: key, imageID: id, exp: tt.exp})
				}
			}

			computeImageSignatures(requests)

			for _, r := range requests {
				imageSignatures.mu.RLock()
				got := imageSignatures.cachedLocked(r.cacheKey())
				imageSignatures.mu.RUnlock()
				if got == nil {
					t.Fatalf("signature for %s not cached", r.imageID)
				}
				if want := referenceImageSignature(r.key, r.imageID, r.exp); !bytes.Equal(got, want) {
					t.Errorf("signature for %s (exp %d) differs from the reference", r.imageID, r.exp)
				}
			}
		})
	}
}
