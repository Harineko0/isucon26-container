package jpegcheck

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"math/rand/v2"
	"os/exec"
	"testing"
)

// cjpegVariants encodes a noisy test image with libjpeg's cjpeg (when it is
// installed) in the subsampling / restart / progressive / optimised-table
// combinations that image/jpeg's encoder cannot produce. It returns nil when
// cjpeg is unavailable.
func cjpegVariants(tb testing.TB) map[string][]byte {
	tb.Helper()
	if _, err := exec.LookPath("cjpeg"); err != nil {
		tb.Log("cjpeg not installed; skipping libjpeg variants")
		return nil
	}
	rng := rand.New(rand.NewPCG(7, 8))
	out := map[string][]byte{}
	for _, sz := range [][2]int{{17, 23}, {120, 80}, {257, 193}} {
		for _, gray := range []bool{false, true} {
			var ppm bytes.Buffer
			if gray {
				fmt.Fprintf(&ppm, "P5\n%d %d\n255\n", sz[0], sz[1])
			} else {
				fmt.Fprintf(&ppm, "P6\n%d %d\n255\n", sz[0], sz[1])
			}
			for y := 0; y < sz[1]; y++ {
				for x := 0; x < sz[0]; x++ {
					if gray {
						ppm.WriteByte(uint8(x*3 + y + rng.IntN(64)))
					} else {
						ppm.Write([]byte{uint8(x + rng.IntN(32)), uint8(y + rng.IntN(32)), uint8(rng.IntN(256))})
					}
				}
			}
			for _, opt := range [][]string{
				{"-quality", "50"},
				{"-quality", "95"},
				{"-quality", "100", "-optimize"},
				{"-quality", "90", "-sample", "1x1"},
				{"-quality", "90", "-sample", "2x1"},
				{"-quality", "90", "-sample", "1x2"},
				{"-quality", "90", "-sample", "2x2", "-restart", "1B"},
				{"-quality", "90", "-sample", "1x1", "-restart", "3B"},
				{"-quality", "90", "-restart", "1"},
				{"-quality", "80", "-progressive"},
				{"-quality", "80", "-progressive", "-restart", "2B"},
				{"-quality", "90", "-baseline", "-optimize", "-restart", "1B"},
			} {
				args := append([]string{}, opt...)
				cmd := exec.Command("cjpeg", args...)
				cmd.Stdin = bytes.NewReader(ppm.Bytes())
				data, err := cmd.Output()
				if err != nil {
					tb.Logf("cjpeg %v: %v (skipped)", args, err)
					continue
				}
				out[fmt.Sprintf("cjpeg_%dx%d_gray=%v_%v", sz[0], sz[1], gray, opt)] = data
			}
		}
	}
	return out
}

// TestValidateMatchesDecodeOnCjpegVariants compares Validate with
// image/jpeg.Decode on libjpeg-encoded images (4:4:4, 4:2:2, 4:4:0, 4:2:0,
// grayscale, restart intervals, progressive, optimised tables) and on
// truncations and random corruptions of each.
func TestValidateMatchesDecodeOnCjpegVariants(t *testing.T) {
	variants := cjpegVariants(t)
	if len(variants) == 0 {
		t.Skip("no cjpeg")
	}
	iterations := 60
	if testing.Short() {
		iterations = 15
	}
	rng := rand.New(rand.NewPCG(5, 6))
	n := 0
	for name, data := range variants {
		if !stdOK(data) {
			// image/jpeg itself rejects some libjpeg outputs (e.g. progressive
			// with restart intervals); what matters is that Validate agrees.
			t.Logf("%s: image/jpeg rejects the cjpeg output", name)
		}
		assertSame(t, name, data)
		for cut := 0; cut < len(data); cut += 1 + cut/16 {
			assertSame(t, name+"/trunc", data[:cut])
			n++
		}
		for i := 0; i < iterations; i++ {
			mut := bytes.Clone(data)
			switch rng.IntN(5) {
			case 0:
				mut[rng.IntN(len(mut))] ^= byte(1 + rng.IntN(255))
			case 1:
				mut[rng.IntN(min(len(mut), 700))] ^= byte(1 + rng.IntN(255))
			case 2:
				p := rng.IntN(len(mut))
				mut = append(mut[:p], append([]byte{byte(rng.IntN(256))}, mut[p:]...)...)
			case 3:
				p := rng.IntN(len(mut))
				mut = append(mut[:p], mut[p+1:]...)
			case 4: // plant a marker-like or stuffing-like byte pair in the scan data
				p := rng.IntN(len(mut))
				mut[p] = 0xff
				if p+1 < len(mut) {
					mut[p+1] = []byte{0x00, 0xd0, 0xd7, 0xd9, 0xff, 0xc4}[rng.IntN(6)]
				}
			}
			assertSame(t, name+"/mut", mut)
			n++
		}
	}
	t.Logf("%d libjpeg variants, %d corrupted inputs compared", len(variants), n)
}

// TestValidateMatchesDecodeOnSyntheticSizes covers many image sizes (partial
// MCUs on both axes) and qualities with Go's own encoder.
func TestValidateMatchesDecodeOnSyntheticSizes(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	n := 0
	for _, q := range []int{1, 30, 75, 92, 100} {
		for w := 1; w <= 40; w += 1 + w/4 {
			for h := 1; h <= 40; h += 1 + h/3 {
				data := encodeNoise(t, w, h, q, rng)
				assertSame(t, fmt.Sprintf("synthetic %dx%d q%d", w, h, q), data)
				for i := 0; i < 3; i++ {
					mut := bytes.Clone(data)
					mut[rng.IntN(len(mut))] ^= byte(1 + rng.IntN(255))
					assertSame(t, "synthetic/mut", mut)
					n++
				}
			}
		}
	}
	t.Logf("%d synthetic corruptions compared", n)
}

func encodeNoise(tb testing.TB, w, h, quality int, rng *rand.Rand) []byte {
	tb.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.IntN(256))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}
