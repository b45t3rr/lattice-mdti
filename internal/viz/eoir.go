// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package viz

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand/v2"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// EOIRSnapshot renders a clearly labelled SIMULATED electro-optical frame: a
// noisy thermal-style background with a reticle and a caption. The simulator
// has no camera, so the image must never be mistaken for real imagery.
func EOIRSnapshot(lines []string, seed uint64) []byte {
	const w, h = 640, 400
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewPCG(seed, 0xe0))
	for y := range h {
		for x := range w {
			// Low-contrast "sea/terrain" gradient with sensor noise.
			v := 40 + 25*math.Sin(float64(x)/90) + 15*math.Cos(float64(y)/60) + rng.NormFloat64()*9
			g := uint8(max(0, min(255, v)))
			img.Set(x, y, color.RGBA{g, g, g, 255})
		}
	}
	green := color.RGBA{120, 255, 140, 255}
	cx, cy := w/2, h/2
	for i := -60; i <= 60; i++ {
		if i < -12 || i > 12 {
			img.Set(cx+i, cy, green)
			img.Set(cx, cy+i, green)
		}
	}
	for _, r := range []int{40, 90} {
		for a := 0.0; a < 2*math.Pi; a += 0.004 {
			img.Set(cx+int(float64(r)*math.Cos(a)), cy+int(float64(r)*math.Sin(a)), green)
		}
	}
	d := &font.Drawer{Dst: img, Src: image.NewUniform(green), Face: basicfont.Face7x13}
	for i, s := range append([]string{"SIMULATED EO/IR - NOT REAL IMAGERY"}, lines...) {
		d.Dot = fixed.P(12, 22+i*16)
		d.DrawString(s)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
