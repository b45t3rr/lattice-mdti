// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package viz

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Series is one line of a chart: Y values at evenly spaced X steps.
type Series struct {
	Name  string
	Color RGBA
	Y     []float64
}

// LineChart renders a small evidence chart as PNG (dark background, axis
// labels, legend). xLabel describes the evenly spaced X axis.
func LineChart(title, xLabel, yLabel string, series []Series) []byte {
	const w, h = 640, 360
	const left, right, top, bottom = 70, 20, 40, 50
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bg := color.RGBA{17, 24, 32, 255}
	grid := color.RGBA{40, 52, 66, 255}
	text := color.RGBA{215, 224, 234, 255}
	for y := range h {
		for x := range w {
			img.Set(x, y, bg)
		}
	}
	lo, hi, n := math.Inf(1), math.Inf(-1), 0
	for _, s := range series {
		for _, v := range s.Y {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		n = max(n, len(s.Y))
	}
	if n < 2 || math.IsInf(lo, 0) {
		return encode(img)
	}
	if hi-lo < 1e-9 {
		hi, lo = hi+1, lo-1
	}
	pad := (hi - lo) * 0.08
	lo, hi = lo-pad, hi+pad
	px := func(i int) int { return left + i*(w-left-right)/(n-1) }
	py := func(v float64) int { return top + int((hi-v)/(hi-lo)*float64(h-top-bottom)) }

	d := &font.Drawer{Dst: img, Src: image.NewUniform(text), Face: basicfont.Face7x13}
	say := func(x, y int, s string) { d.Dot = fixed.P(x, y); d.DrawString(s) }
	for k := 0; k <= 4; k++ {
		v := lo + (hi-lo)*float64(k)/4
		y := py(v)
		for x := left; x < w-right; x++ {
			img.Set(x, y, grid)
		}
		say(6, y+4, fmt.Sprintf("%7.0f", v))
	}
	for _, s := range series {
		c := color.RGBA{uint8(s.Color.R), uint8(s.Color.G), uint8(s.Color.B), 255}
		for i := 1; i < len(s.Y); i++ {
			line(img, px(i-1), py(s.Y[i-1]), px(i), py(s.Y[i]), c)
		}
	}
	say(left, 22, title)
	say(left, h-12, xLabel)
	say(6, top-8, yLabel)
	lx := w - right - 10
	for i := len(series) - 1; i >= 0; i-- {
		s := series[i]
		lx -= 7*len(s.Name) + 30
		c := color.RGBA{uint8(s.Color.R), uint8(s.Color.G), uint8(s.Color.B), 255}
		for x := lx; x < lx+16; x++ {
			img.Set(x, h-17, c)
			img.Set(x, h-16, c)
		}
		say(lx+20, h-12, s.Name)
	}
	say(w-right-7*len("SIMULATED DATA")-4, 22, "SIMULATED DATA")
	return encode(img)
}

// line draws a 2-px line (Bresenham).
func line(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	dx, dy := abs(x1-x0), -abs(y1-y0)
	sx, sy := sign(x1-x0), sign(y1-y0)
	e := dx + dy
	for {
		img.Set(x0, y0, c)
		img.Set(x0, y0+1, c)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * e
		if e2 >= dy {
			e += dy
			x0 += sx
		}
		if e2 <= dx {
			e += dx
			y0 += sy
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

func encode(img image.Image) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
