// Command icon renders the add-on icon (ha-lua/icon.png).
//
// The glyph is a nod to both projects: an orbiting moon on an arc
// (Lua's planet-and-moon mark) around a planet carrying a house
// cutout (Home Assistant). Rendered analytically per pixel with
// supersampling — no font, no SVG rasterizer, no dependencies.
//
// Usage: go run ./tools/icon <output.png>
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const (
	size    = 128 // HA add-on icons are 128x128
	samples = 4   // supersamples per axis (16 per pixel)

	cornerRadius = 28

	planetX = 56
	planetY = 74
	planetR = 37

	moonX = 99
	moonY = 29
	moonR = 13
)

type rgb struct{ r, g, b float64 }

func hex(v uint32) rgb {
	return rgb{
		float64(v>>16&0xff) / 255,
		float64(v>>8&0xff) / 255,
		float64(v&0xff) / 255,
	}
}

func mix(a, b rgb, t float64) rgb {
	return rgb{
		a.r + (b.r-a.r)*t,
		a.g + (b.g-a.g)*t,
		a.b + (b.b-a.b)*t,
	}
}

var (
	bgTop    = hex(0x14386E) // night sky, lighter at the top
	bgBottom = hex(0x081A3A)
	planet   = hex(0x18BCF2) // Home Assistant brand blue
	house    = hex(0x081A3A) // house cutout shows the night sky
	moon     = hex(0xEAF6FF)
	orbit    = hex(0xFFFFFF)
)

// insideTile reports whether the point is inside the rounded-square tile.
func insideTile(x, y float64) bool {
	half := float64(size) / 2
	dx := math.Max(math.Abs(x-half)-(half-cornerRadius), 0)
	dy := math.Max(math.Abs(y-half)-(half-cornerRadius), 0)
	return dx*dx+dy*dy <= cornerRadius*cornerRadius
}

func insideCircle(x, y, cx, cy, r float64) bool {
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
}

// insideHouse tests the pentagon house glyph centered on the planet:
// a roof triangle with a slight overhang on top of a rectangular body.
func insideHouse(x, y float64) bool {
	const (
		apexY    = 54
		roofY    = 72
		roofHalf = 20
		bodyHalf = 17
		bodyBot  = 90
	)
	if y >= roofY && y <= bodyBot {
		return math.Abs(x-planetX) <= bodyHalf
	}
	if y >= apexY && y < roofY {
		halfWidth := (y - apexY) / (roofY - apexY) * roofHalf
		return math.Abs(x-planetX) <= halfWidth
	}
	return false
}

// onOrbit tests the thin arc the moon travels on: a ring around the
// planet, limited to the sector pointing at the moon, with a small
// gap around the moon itself.
func onOrbit(x, y float64) bool {
	dx, dy := x-planetX, y-planetY
	dist := math.Hypot(dx, dy)
	orbitR := math.Hypot(moonX-planetX, moonY-planetY)
	if math.Abs(dist-orbitR) > 1.4 {
		return false
	}
	if insideCircle(x, y, moonX, moonY, moonR+4) {
		return false
	}
	moonDirX := (moonX - planetX) / orbitR
	moonDirY := (moonY - planetY) / orbitR
	cosAngle := (dx*moonDirX + dy*moonDirY) / dist
	return cosAngle > 0.45
}

// sample returns the color and coverage of a single supersample point.
func sample(x, y float64) (rgb, float64) {
	if !insideTile(x, y) {
		return rgb{}, 0
	}
	col := mix(bgTop, bgBottom, y/size)
	if onOrbit(x, y) {
		col = mix(col, orbit, 0.4)
	}
	if insideCircle(x, y, planetX, planetY, planetR) {
		col = planet
		if insideHouse(x, y) {
			col = house
		}
	}
	if insideCircle(x, y, moonX, moonY, moonR) {
		col = moon
	}
	return col, 1
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: icon <output.png>")
		os.Exit(1)
	}

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	step := 1.0 / samples
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var acc rgb
			var alpha float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					col, a := sample(
						float64(px)+(float64(sx)+0.5)*step,
						float64(py)+(float64(sy)+0.5)*step,
					)
					acc.r += col.r * a
					acc.g += col.g * a
					acc.b += col.b * a
					alpha += a
				}
			}
			n := float64(samples * samples)
			if alpha == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(acc.r/alpha*255 + 0.5),
				G: uint8(acc.g/alpha*255 + 0.5),
				B: uint8(acc.b/alpha*255 + 0.5),
				A: uint8(alpha/n*255 + 0.5),
			})
		}
	}

	out, err := os.Create(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := png.Encode(out, img); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := out.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
