package jpegs

import (
	"bytes"
	"image/jpeg"
	"math/rand/v2"
	"testing"
)

func TestGenerateDecodes(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	img := GenerateSized(rng, 1024, 768, 350*1024)
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 1024 || cfg.Height != 768 {
		t.Fatalf("size = %dx%d", cfg.Width, cfg.Height)
	}
	if n := len(img.Data); n < 200*1024 || n > 600*1024 {
		t.Fatalf("size %d bytes is far from the 350 KB target", n)
	}
	small := Generate(rng, 320, 320, 85, 0.3)
	if _, err := jpeg.Decode(bytes.NewReader(small.Data)); err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(Tiny(rng).Data)); err != nil {
		t.Fatal(err)
	}
}

func TestPoolDistinct(t *testing.T) {
	p := NewPool(rand.New(rand.NewPCG(3, 4)), 4, 64, 48, 0)
	seen := map[[32]byte]bool{}
	for _, img := range p.images {
		seen[img.SHA256] = true
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 distinct images, got %d", len(seen))
	}
}
