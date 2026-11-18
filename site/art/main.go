//go:build ignore

// Generates the site's stippled hero artwork. Run from the repo root:
//
//	go run site/art/main.go
//
// It writes site/art/hero.png: a lighthouse at dusk on a headland, its amber
// beam running level across the frame, ordered- and randomly dithered into
// navy dots on fog. On the site, the demo card sits in the beam's path.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

const (
	width, height = 1040, 1200 // output pixels
	cell          = 3          // one dot per 3×3 block
)

var (
	fog   = color.RGBA{0xDC, 0xE4, 0xEA, 0xFF}
	navy  = color.RGBA{0x13, 0x30, 0x4F, 0xFF}
	amber = color.RGBA{0xF2, 0xA9, 0x3B, 0xFF}
)

// bayer8 is the classic 8×8 ordered-dither matrix.
var bayer8 = [8][8]float64{
	{0, 32, 8, 40, 2, 34, 10, 42}, {48, 16, 56, 24, 50, 18, 58, 26},
	{12, 44, 4, 36, 14, 46, 6, 38}, {60, 28, 52, 20, 62, 30, 54, 22},
	{3, 35, 11, 43, 1, 33, 9, 41}, {51, 19, 59, 27, 49, 17, 57, 25},
	{15, 47, 7, 39, 13, 45, 5, 37}, {63, 31, 55, 23, 61, 29, 53, 21},
}

// ---------------------------------------------------------------------------
// Math helpers

func hash(x, y int) float64 {
	h := uint32(x)*374761393 + uint32(y)*668265263
	h = (h ^ (h >> 13)) * 1274126177
	return float64(h^(h>>16)) / float64(math.MaxUint32)
}

// noise is smooth value noise in [0,1].
func noise(x, y float64) float64 {
	xi, yi := math.Floor(x), math.Floor(y)
	fx, fy := x-xi, y-yi
	sx, sy := fx*fx*(3-2*fx), fy*fy*(3-2*fy)
	a, b := hash(int(xi), int(yi)), hash(int(xi)+1, int(yi))
	c, d := hash(int(xi), int(yi)+1), hash(int(xi)+1, int(yi)+1)
	return a + (b-a)*sx + (c-a)*sy + (a-b-c+d)*sx*sy
}

func fbm(x, y float64) float64 {
	sum, amp, norm := 0.0, 0.5, 0.0
	for range 5 {
		sum += amp * noise(x, y)
		norm += amp
		x, y, amp = x*2.03, y*2.03, amp*0.5
	}
	return sum / norm
}

func clamp(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func smoothstep(a, b, v float64) float64 {
	t := clamp((v - a) / (b - a))
	return t * t * (3 - 2*t)
}

// ---------------------------------------------------------------------------
// The scene: a lighthouse at dusk on a headland. Its amber beam runs level
// across the frame, through where the page's demo card sits, and fades out
// past it.

func scene(u, v float64) (float64, float64) {
	horizon := 0.64
	lx, ly := 0.10, 0.47 // the lamp

	// Sky: darker at the top, pale toward the horizon, with drifting cloud banks.
	sky := 0.58 - 0.52*smoothstep(0, horizon, v)
	sky += (fbm(u*2.6+0.3, v*6.5) - 0.5) * 1.1 * (1 - 0.7*v/horizon)
	density := math.Max(sky, 0.1+0.08*fbm(u*14, v*3)) // a faint haze near the horizon
	beam := 0.0

	// Sea: banded swell under the horizon, darker toward the viewer.
	if v > horizon {
		depth := (v - horizon) / (1 - horizon)
		swell := math.Sin(v*170+fbm(u*5, v*24)*9) * 0.2
		density = 0.32 + depth*0.5 + swell + (fbm(u*9, v*40)-0.5)*0.25
	}

	// Headland: a rough hill crowned by the tower, falling away to the sea.
	ridge := 0.605 + math.Pow(math.Abs(u-lx), 1.3)*0.9 + (fbm(u*9, 3.1)-0.5)*0.06
	if v > ridge && u < lx+0.55 {
		density = 0.82 + (fbm(u*24, v*24)-0.5)*0.35
	}

	// Tower: a tapering column with two painted bands.
	const base = 0.615
	if v > ly+0.012 && v < base {
		half := 0.014 + (v-ly)*0.06
		if math.Abs(u-lx) < half {
			density = 0.96
			if band := (v - ly) / (base - ly); (band > 0.3 && band < 0.42) || (band > 0.66 && band < 0.78) {
				density = 0.12
			}
		}
	}
	// Lantern roof, then the lamp itself.
	if v > ly-0.03 && v < ly-0.014 && math.Abs(u-lx) < 0.026*(v-(ly-0.03))/0.016 {
		density = 0.95
	}
	if math.Abs(u-lx) < 0.022 && math.Abs(v-ly) < 0.016 {
		density, beam = 0, 1
	}

	// Beam: a widening wedge from the lamp, running level to the right.
	if dx, dy := u-lx, v-ly; dx > 0 {
		center, spread := -0.03, 0.04+dx*0.16
		if off := math.Abs(math.Atan2(dy, dx) - center); off < spread {
			strength := (1 - off/spread) * (1 - smoothstep(0.45, 0.95, dx))
			beam = math.Max(beam, strength)
			density *= 1 - strength*0.9
		}
	}
	return clamp(density), clamp(beam)
}

func render(name string, scene func(u, v float64) (float64, float64)) {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, fog)
		}
	}
	for cy := 0; cy < height/cell; cy++ {
		for cx := 0; cx < width/cell; cx++ {
			u := (float64(cx) + 0.5) / float64(width/cell)
			v := (float64(cy) + 0.5) / float64(height/cell)
			d, g := scene(u, v)
			// Mostly random threshold for an organic grain, a little ordered for structure.
			t := 0.75*hash(cx*7+11, cy*13+5) + 0.25*(bayer8[cy%8][cx%8]+0.5)/64
			var c color.RGBA
			switch {
			case g > t*0.9:
				c = amber
			case d > t:
				c = navy
			default:
				continue
			}
			// A 2×2 dot inside each 3×3 cell keeps the stipple texture.
			for py := range 2 {
				for px := range 2 {
					img.Set(cx*cell+px, cy*cell+py, c)
				}
			}
		}
	}
	f, err := os.Create(name)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", name)
}

func main() {
	render("site/art/hero.png", scene)
}
