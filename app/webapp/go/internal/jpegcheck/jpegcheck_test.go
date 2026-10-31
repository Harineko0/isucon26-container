package jpegcheck

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// corpus returns valid JPEGs: the image/jpeg test images shipped with GOROOT
// (baseline, progressive, gray, CMYK, RGB, every subsampling ratio, restart
// intervals), synthetic baseline images, and any *.jpg/*.jpeg files in the
// directory named by JPEGCHECK_CORPUS (e.g. real uploads).
func corpus(tb testing.TB) map[string][]byte {
	tb.Helper()
	out := map[string][]byte{}
	var files []string
	for _, pattern := range []string{
		filepath.Join(runtime.GOROOT(), "src", "image", "testdata", "*.jpeg"),
		filepath.Join(os.Getenv("JPEGCHECK_CORPUS"), "*.jpg"),
		filepath.Join(os.Getenv("JPEGCHECK_CORPUS"), "*.jpeg"),
	} {
		m, _ := filepath.Glob(pattern)
		files = append(files, m...)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			tb.Fatal(err)
		}
		out[filepath.Base(f)] = data
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for _, sz := range [][2]int{{1, 1}, {7, 9}, {64, 48}, {513, 300}, {300, 513}} {
		for _, gray := range []bool{false, true} {
			var img image.Image
			if gray {
				g := image.NewGray(image.Rect(0, 0, sz[0], sz[1]))
				for i := range g.Pix {
					g.Pix[i] = uint8(rng.IntN(256))
				}
				img = g
			} else {
				c := image.NewRGBA(image.Rect(0, 0, sz[0], sz[1]))
				for y := 0; y < sz[1]; y++ {
					for x := 0; x < sz[0]; x++ {
						c.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(rng.IntN(256)), 255})
					}
				}
				img = c
			}
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
				tb.Fatal(err)
			}
			out["synthetic"+string(rune('a'+len(out)))] = buf.Bytes()
		}
	}
	return out
}

// stdOK reports whether image/jpeg.Decode accepts data. A panic counts as a
// rejection: the handlers would not have stored such an image either.
func stdOK(data []byte) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	_, err := jpeg.Decode(bytes.NewReader(data))
	return err == nil
}

func assertSame(t *testing.T, name string, data []byte) {
	t.Helper()
	want := stdOK(data)
	w, h, err := Validate(bytes.NewReader(data))
	if got := err == nil; got != want {
		t.Fatalf("%s (len %d): Validate ok=%v (err=%v), image/jpeg.Decode ok=%v", name, len(data), got, err, want)
	}
	if err == nil {
		cfg, cerr := jpeg.DecodeConfig(bytes.NewReader(data))
		if cerr != nil || cfg.Width != w || cfg.Height != h {
			t.Fatalf("%s: Validate %dx%d, DecodeConfig %dx%d (err=%v)", name, w, h, cfg.Width, cfg.Height, cerr)
		}
	}
}

func TestValidateMatchesDecodeOnCorpus(t *testing.T) {
	for name, data := range corpus(t) {
		assertSame(t, name, data)
	}
	for _, data := range [][]byte{nil, {0xff}, {0xff, 0xd8}, {0xff, 0xd8, 0xff, 0xd9}, []byte("not a jpeg at all")} {
		assertSame(t, "tiny", data)
	}
}

// TestValidateMatchesDecodeOnCorruptions compares Validate with image/jpeg.Decode
// on truncations, byte flips, insertions and deletions of every corpus image.
func TestValidateMatchesDecodeOnCorruptions(t *testing.T) {
	iterations := 100
	if testing.Short() {
		iterations = 30
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for name, data := range corpus(t) {
		// Truncations: dense near the headers, sparse in the scan data.
		for n := 0; n < len(data); n += 1 + n/8 {
			assertSame(t, name+"/trunc", data[:n])
		}
		for i := 0; i < iterations; i++ {
			mut := bytes.Clone(data)
			switch rng.IntN(4) {
			case 0: // flip one byte anywhere
				mut[rng.IntN(len(mut))] ^= byte(1 + rng.IntN(255))
			case 1: // flip one byte in the first 1 KiB (markers, tables)
				mut[rng.IntN(min(len(mut), 1024))] ^= byte(1 + rng.IntN(255))
			case 2: // insert a byte
				p := rng.IntN(len(mut))
				mut = append(mut[:p], append([]byte{byte(rng.IntN(256))}, mut[p:]...)...)
			case 3: // delete a byte
				p := rng.IntN(len(mut))
				mut = append(mut[:p], mut[p+1:]...)
			}
			assertSame(t, name+"/mut", mut)
		}
	}
}

func FuzzValidate(f *testing.F) {
	for _, data := range corpus(f) {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		assertSame(t, "fuzz", data)
	})
}

func benchCorpus(b *testing.B) [][]byte {
	dir := os.Getenv("JPEGCHECK_CORPUS")
	if dir == "" {
		b.Skip("set JPEGCHECK_CORPUS to a directory of real upload JPEGs")
	}
	var out [][]byte
	m, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	for _, f := range m {
		data, err := os.ReadFile(f)
		if err != nil {
			b.Fatal(err)
		}
		out = append(out, data)
	}
	return out
}

func BenchmarkStdDecode(b *testing.B) {
	imgs := benchCorpus(b)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, err := jpeg.Decode(bytes.NewReader(imgs[i%len(imgs)])); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidate(b *testing.B) {
	imgs := benchCorpus(b)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, _, err := Validate(bytes.NewReader(imgs[i%len(imgs)])); err != nil {
			b.Fatal(err)
		}
	}
}
