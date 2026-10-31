package jpegcheck

import (
	"bytes"
	"image/jpeg"
	"strings"
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
			// AC 0 => run (one extra bit), AC 10 => EOB.
			ac := make([]byte, 19)
			ac[0], ac[1], ac[2], ac[17] = 0x10, 1, 1, 0x10
			data := grayscaleScan(16, ac, tc.entropy, tc.restart)
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

func grayscaleScan(width byte, ac, entropy []byte, restart bool) []byte {
	data := []byte{0xff, 0xd8}
	segment := func(marker byte, body []byte) {
		n := len(body) + 2
		data = append(data, 0xff, marker, byte(n>>8), byte(n))
		data = append(data, body...)
	}
	segment(0xdb, append([]byte{0}, bytes.Repeat([]byte{1}, 64)...))
	segment(0xc0, []byte{8, 0, 8, 0, width, 1, 1, 0x11, 0})
	dc := make([]byte, 18)
	dc[1] = 1 // DC 0 => size 0.
	segment(0xc4, append(dc, ac...))
	if restart {
		segment(0xdd, []byte{0, 1})
	}
	segment(0xda, []byte{1, 1, 0, 0, 63, 0})
	data = append(data, entropy...)
	return append(data, 0xff, 0xd9)
}

func TestSequentialACPairAtBlockEnd(t *testing.T) {
	// AC 0 => one coefficient, 10 => skip 15 then one coefficient,
	// 110 => EOB. The first block ends with an unpaired coefficient.
	// The fourth long run in block two overshoots and has no amplitude.
	ac := make([]byte, 20)
	ac[0], ac[1], ac[2], ac[3] = 0x10, 1, 1, 1
	ac[17], ac[18], ac[19] = 0x01, 0xf1, 0x00
	dense := "0" + strings.Repeat("01", 63)
	scan := dense + "0" + strings.Repeat("101", 3) + "10" + dense + "0110"
	var entropy []byte
	for len(scan) > 0 {
		n := min(8, len(scan))
		word := byte(0xff)
		for i := 0; i < n; i++ {
			if scan[i] == '0' {
				word &^= 0x80 >> i
			}
		}
		entropy = append(entropy, word)
		if word == 0xff {
			entropy = append(entropy, 0)
		}
		scan = scan[n:]
	}
	data := grayscaleScan(32, ac, entropy, false)
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	for end := range len(entropy) + 1 {
		assertSame(t, "coefficient pair at block end", grayscaleScan(32, ac, entropy[:end], false))
	}
}

// A later length-8 code overwrites one entry of a shorter code in the
// standard 8-bit LUT. A partially known second prefix cannot resolve it.
func TestSequentialOversubscribedACTable(t *testing.T) {
	ac := make([]byte, 21)
	ac[0], ac[1], ac[2], ac[8] = 0x10, 1, 2, 1
	ac[17], ac[18], ac[19], ac[20] = 0x01, 0x02, 0x03, 0x04
	var d decoder
	d.bytes.buf, d.bytes.j = ac, len(ac)
	if err := d.processDHT(len(ac)); err != nil {
		t.Fatal(err)
	}
	for i, entry := range d.huff[acTable][0].seq {
		if entry&seqAdvanceFlag != 0 {
			t.Fatalf("oversubscribed prefix %010b used an AC shortcut", i)
		}
	}
}
