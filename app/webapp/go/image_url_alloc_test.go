package isureal

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"testing"
)

func oldImageURLParts(id string, exp int64, sig []byte) string {
	query := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "sig": {base64.RawURLEncoding.EncodeToString(sig)}}
	return "/api/images/" + url.PathEscape(id) + "?" + query.Encode()
}

func TestImageURLPartsCompatibility(t *testing.T) {
	for _, id := range []string{"", "018d8d55-a1fa-4000-8000-000000000001", "path/with ?query&=+%日本語"} {
		for _, exp := range []int64{-1, 0, 1793500000, 1<<63 - 1} {
			for _, sig := range [][]byte{nil, {0, 1, 255, 254}, make([]byte, 32)} {
				got, want := signedImageURLParts(id, exp, sig), oldImageURLParts(id, exp, sig)
				if got != want {
					t.Fatalf("got %q, want %q", got, want)
				}
			}
		}
	}
}

var imageURLSink string
var signaturePlanSink *signaturePlan

func BenchmarkImageURLParts(b *testing.B) {
	for _, tc := range []struct {
		name string
		fn   func(string, int64, []byte) string
	}{{"old", oldImageURLParts}, {"new", signedImageURLParts}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			sig := make([]byte, 32)
			for b.Loop() {
				imageURLSink = tc.fn("018d8d55-a1fa-4000-8000-000000000001", 1793500000, sig)
			}
		})
	}
}

func BenchmarkSignaturePlanCapacity(b *testing.B) {
	for _, size := range []int{0, 100} {
		name := "old"
		if size > 0 {
			name = "reserved"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				p := newSignaturePlan(size)
				for range 100 {
					p.add(User{}, "image")
				}
				signaturePlanSink = p
			}
		})
	}
}
