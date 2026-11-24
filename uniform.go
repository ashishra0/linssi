package linssi

import (
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
)

// noInk reports whether every image is one flat color: no text, edges or
// media anywhere, even though the DOM has some. A page with a header and a
// few words is already ~99.9% background, so the bar is ~99.99%.
func noInk(images [][]byte) bool {
	for _, img := range images {
		if uniformShare(img) < 0.9999 {
			return false
		}
	}
	return len(images) > 0
}

// uniformShare decodes a PNG or JPEG and returns the share of sampled pixels
// that fall in its most common color (4 bits per channel). An image it can't
// decode (WebP) returns 0.
func uniformShare(data []byte) float64 {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return 0
	}
	b := img.Bounds()
	step := max(1, b.Dx()/600)
	counts := map[uint32]int{}
	total, top := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, _ := img.At(x, y).RGBA()
			k := r>>12<<8 | g>>12<<4 | bl>>12
			counts[k]++
			top = max(top, counts[k])
			total++
		}
	}
	return float64(top) / float64(total)
}
