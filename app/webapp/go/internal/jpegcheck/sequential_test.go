package jpegcheck

import (
	"bytes"
	"image/jpeg"
	"testing"
	"testing/iotest"
)

// Two grayscale blocks. The first AC code is an end-of-band run of two, which
// image/jpeg accepts in sequential images. The next block must still consume DC.
// A restart between blocks discards the outstanding run and requires AC again.
func TestSequentialEndOfBandAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
		entropy []byte
		valid   bool
	}{
		{"continued run", false, []byte{0x0f}, true},
		{"reset run", true, []byte{0x1f, 0xff, 0xd0, 0x5f}, true},
		{"missing DC", false, nil, false},
		{"missing second scan data", true, []byte{0x1f, 0xff, 0xd0}, false},
		{"wrong restart", true, []byte{0x1f, 0xff, 0xd1, 0x5f}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte{0xff, 0xd8}
			segment := func(marker byte, body []byte) {
				n := len(body) + 2
				data = append(data, 0xff, marker, byte(n>>8), byte(n))
				data = append(data, body...)
			}
			segment(0xdb, append([]byte{0}, bytes.Repeat([]byte{1}, 64)...))
			segment(0xc0, []byte{8, 0, 8, 0, 16, 1, 1, 0x11, 0})
			// DC 0 => size 0. AC 0 => run (one extra bit), AC 10 => EOB.
			dc := make([]byte, 18)
			dc[1] = 1
			ac := make([]byte, 19)
			ac[0], ac[1], ac[2], ac[17] = 0x10, 1, 1, 0x10
			segment(0xc4, append(dc, ac...))
			if tc.restart {
				segment(0xdd, []byte{0, 1})
			}
			segment(0xda, []byte{1, 1, 0, 0, 63, 0})
			data = append(data, tc.entropy...)
			data = append(data, 0xff, 0xd9)
			if _, err := jpeg.Decode(bytes.NewReader(data)); (err == nil) != tc.valid {
				t.Fatalf("reference got %v, want valid=%t", err, tc.valid)
			}
			assertSame(t, tc.name, data)
			_, _, err := Validate(iotest.OneByteReader(bytes.NewReader(data)))
			if (err == nil) != tc.valid {
				t.Fatalf("split stream got %v, want valid=%t", err, tc.valid)
			}
		})
	}
}
