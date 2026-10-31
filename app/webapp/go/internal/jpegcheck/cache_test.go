package jpegcheck

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/jpeg"
	"math/rand/v2"
	"sync"
	"testing"
)

func TestValidationCacheMatchesDecoder(t *testing.T) {
	c := newValidationCache(8)
	rng := rand.New(rand.NewPCG(12, 34))
	for name, data := range corpus(t) {
		inputs := [][]byte{data, data[:len(data)/2]}
		for range 10 {
			mut := bytes.Clone(data)
			mut[rng.IntN(len(mut))] ^= 0xff
			inputs = append(inputs, mut)
		}
		for _, input := range inputs {
			// Run twice to cover both a miss and a hit for valid content.
			for range 2 {
				w, h, err := c.validate(input)
				if (err == nil) != stdOK(input) {
					t.Fatalf("%s: cache %v disagrees with image/jpeg.Decode", name, err)
				}
				if err == nil {
					cfg, _ := jpeg.DecodeConfig(bytes.NewReader(input))
					if w != cfg.Width || h != cfg.Height {
						t.Fatalf("%s: wrong cached dimensions %dx%d", name, w, h)
					}
				}
			}
		}
	}
}

func TestValidationCacheCoversEntireInput(t *testing.T) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 64, 64)), nil)
	data := buf.Bytes()
	c := newValidationCache(2)
	for range 2 {
		if _, _, err := c.validate(data); err != nil {
			t.Fatal(err)
		}
	}
	if c.hits.Load() != 1 || c.misses.Load() != 1 {
		t.Fatal("a repeat upload must reuse the complete validation")
	}
	// Leave every header byte and the length intact, corrupt just the ending.
	mut := bytes.Clone(data)
	mut[len(mut)-1] = 0
	if _, _, err := c.validate(mut); err == nil {
		t.Fatal("cached header must not allow corrupted scan data")
	}
	if c.misses.Load() != 2 {
		t.Fatal("same length and headers must not cause a cache hit")
	}
	if len(c.current) != 1 {
		t.Fatal("invalid content must not be cached")
	}
}

func TestValidationCacheBoundAndReset(t *testing.T) {
	c := newValidationCache(2)
	var last [sha256.Size]byte
	for i := range 30 {
		last = sha256.Sum256([]byte{byte(i)})
		c.remember(last, dimensions{512, 513}, 0)
		if len(c.current) > 2 || len(c.previous) > 2 {
			t.Fatal("cache exceeds two bounded generations")
		}
	}
	_, _, before := c.lookup(last)
	c.reset()
	// Simulate a validation completing after the initialize reset.
	c.remember(last, dimensions{512, 513}, before)
	if _, ok, _ := c.lookup(last); ok {
		t.Fatal("pre-reset validation repopulated the cache")
	}
	_, _, after := c.lookup(last)
	c.remember(last, dimensions{512, 513}, after)
	if d, ok, _ := c.lookup(last); !ok || d.height != 513 {
		t.Fatal("new generation did not accept a new validation")
	}
}

func TestValidationCacheConcurrentReset(t *testing.T) {
	c := newValidationCache(2)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				key := sha256.Sum256([]byte{byte(i), byte(j)})
				_, _, generation := c.lookup(key)
				c.remember(key, dimensions{1, 1}, generation)
				if j%10 == 0 {
					c.reset()
				}
			}
		})
	}
	wg.Wait()
	if len(c.current) > 2 || len(c.previous) > 2 {
		t.Fatal("concurrent writes exceeded capacity")
	}
}

func BenchmarkValidationCache(b *testing.B) {
	img := image.NewGray(image.Rect(0, 0, 1024, 1024))
	rng := rand.New(rand.NewPCG(5, 6))
	for i := range img.Pix {
		img.Pix[i] = byte(rng.Uint32())
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		b.Fatal(err)
	}
	data := buf.Bytes()
	for _, mode := range []string{"uncached", "cold", "warm"} {
		b.Run(mode, func(b *testing.B) {
			c := newValidationCache(2048)
			c.validate(data)
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				var err error
				if mode == "uncached" {
					_, _, err = Validate(bytes.NewReader(data))
				} else {
					if mode == "cold" {
						c.reset()
					}
					_, _, err = c.validate(data)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
