// Package jpegs はベンチが投稿する JPEG を作る。
// アプリは JPEG を復号して検証する (post は 4096px、RealMoji・プロフィールは 512px まで) ので、
// 標準ライブラリの baseline JPEG を作る。写真らしい大きさ (本番の投稿画像は 300〜400 KB) に近づけるため
// グラデーションにノイズを混ぜて圧縮しにくくし、ByteTarget に近い大きさを 2 分探索で作る。
package jpegs

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
)

type Image struct {
	Data   []byte
	SHA256 [32]byte
	Width  int
	Height int
}

// Generate は width×height の JPEG を作る。noise (0〜1) が大きいほどファイルが大きい。
func Generate(rng *rand.Rand, width, height int, quality int, noise float64) *Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	base := [3]float64{rng.Float64() * 255, rng.Float64() * 255, rng.Float64() * 255}
	dx := [3]float64{(rng.Float64() - 0.5) * 2, (rng.Float64() - 0.5) * 2, (rng.Float64() - 0.5) * 2}
	dy := [3]float64{(rng.Float64() - 0.5) * 2, (rng.Float64() - 0.5) * 2, (rng.Float64() - 0.5) * 2}
	// 「椅子」風に長方形をいくつか置く (世界グランプリは椅子の写真)
	type rect struct {
		x0, y0, x1, y1 int
		c              [3]uint8
	}
	rects := make([]rect, 3+rng.IntN(5))
	for i := range rects {
		x0, y0 := rng.IntN(width), rng.IntN(height)
		rects[i] = rect{x0, y0, x0 + rng.IntN(width/2+1), y0 + rng.IntN(height/2+1), [3]uint8{uint8(rng.IntN(256)), uint8(rng.IntN(256)), uint8(rng.IntN(256))}}
	}
	amp := noise * 128
	pix := img.Pix
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var c [3]float64
			for k := 0; k < 3; k++ {
				c[k] = base[k] + dx[k]*float64(x)*255/float64(width) + dy[k]*float64(y)*255/float64(height)
			}
			for _, r := range rects {
				if x >= r.x0 && x < r.x1 && y >= r.y0 && y < r.y1 {
					c = [3]float64{float64(r.c[0]), float64(r.c[1]), float64(r.c[2])}
				}
			}
			if amp > 0 {
				n := (rng.Float64() - 0.5) * amp
				c[0] += n
				c[1] += n * 0.7
				c[2] += n * 1.3
			}
			i := y*img.Stride + x*4
			pix[i] = clamp(c[0])
			pix[i+1] = clamp(c[1])
			pix[i+2] = clamp(c[2])
			pix[i+3] = 255
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	data := buf.Bytes()
	return &Image{Data: data, SHA256: sha256.Sum256(data), Width: width, Height: height}
}

func clamp(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// GenerateSized は byteTarget に近い大きさの JPEG を作る (ノイズ量で調整)。
func GenerateSized(rng *rand.Rand, width, height, byteTarget int) *Image {
	if byteTarget <= 0 {
		return Generate(rng, width, height, 80, 0)
	}
	lo, hi := 0.0, 1.0
	seed := rng.Uint64()
	var best *Image
	for i := 0; i < 7; i++ {
		mid := (lo + hi) / 2
		r := rand.New(rand.NewPCG(seed, uint64(i)))
		img := Generate(r, width, height, 85, mid)
		if best == nil || abs(len(img.Data)-byteTarget) < abs(len(best.Data)-byteTarget) {
			best = img
		}
		if len(img.Data) < byteTarget {
			lo = mid
		} else {
			hi = mid
		}
		if abs(len(img.Data)-byteTarget)*10 < byteTarget {
			break
		}
	}
	return best
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Tiny は壊れていない最小級の JPEG (初期データ生成で件数が多いとき用)。
func Tiny(rng *rand.Rand) *Image {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	c := color.RGBA{uint8(rng.IntN(256)), uint8(rng.IntN(256)), uint8(rng.IntN(256)), 255}
	for i := 0; i < 16*16; i++ {
		img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2], img.Pix[i*4+3] = c.R, c.G, c.B, 255
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 50})
	data := buf.Bytes()
	return &Image{Data: data, SHA256: sha256.Sum256(data), Width: 16, Height: 16}
}

// Pool は使い回す画像の束。ベンチ中に毎回エンコードしない。
type Pool struct {
	images []*Image
}

func NewPool(rng *rand.Rand, n, width, height, byteTarget int) *Pool {
	p := &Pool{}
	for i := 0; i < n; i++ {
		w, h := width, height
		if rng.IntN(3) == 0 {
			w, h = h, w // 縦長もまぜる
		}
		p.images = append(p.images, GenerateSized(rng, w, h, byteTarget))
	}
	return p
}

func (p *Pool) Pick(rng *rand.Rand) *Image { return p.images[rng.IntN(len(p.images))] }
