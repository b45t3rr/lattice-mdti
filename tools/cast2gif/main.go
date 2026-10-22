// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command cast2gif renders an asciicast v2 recording (as written by cmd/demo)
// into an animated GIF, with no external tools. It implements just enough of
// a terminal for narrated output: printable ASCII, CR/LF, line wrap, scrolling
// and SGR colours. Idle gaps are compressed so the GIF stays short.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"os"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Palette indices.
const (
	pBG = iota
	pFG
	pDim
	pRed
	pGreen
	pYellow
	pBlue
	pMagenta
	pCyan
	pWhite
	pBar
	pDotR
	pDotY
	pDotG
)

var palette = color.Palette{
	color.RGBA{0x0d, 0x11, 0x17, 0xff}, // background
	color.RGBA{0xc9, 0xd1, 0xd9, 0xff}, // default foreground
	color.RGBA{0x6e, 0x76, 0x81, 0xff}, // dim
	color.RGBA{0xff, 0x7b, 0x72, 0xff},
	color.RGBA{0x7e, 0xe7, 0x87, 0xff},
	color.RGBA{0xe3, 0xb3, 0x41, 0xff},
	color.RGBA{0x79, 0xc0, 0xff, 0xff},
	color.RGBA{0xd2, 0xa8, 0xff, 0xff},
	color.RGBA{0x56, 0xd4, 0xdd, 0xff},
	color.RGBA{0xf0, 0xf6, 0xfc, 0xff}, // bright white (bold)
	color.RGBA{0x16, 0x1b, 0x22, 0xff}, // title bar
	color.RGBA{0xff, 0x5f, 0x57, 0xff},
	color.RGBA{0xfe, 0xbc, 0x2e, 0xff},
	color.RGBA{0x28, 0xc8, 0x40, 0xff},
}

type cell struct {
	r  rune
	fg uint8
}

type term struct {
	w, h  int
	grid  [][]cell
	x, y  int
	fg    uint8
	bold  bool
	dirty bool
}

func newTerm(w, h int) *term {
	t := &term{w: w, h: h, fg: pFG}
	for range h {
		t.grid = append(t.grid, make([]cell, w))
	}
	return t
}

func (t *term) newline() {
	t.x = 0
	if t.y < t.h-1 {
		t.y++
		return
	}
	copy(t.grid, t.grid[1:])
	t.grid[t.h-1] = make([]cell, t.w)
}

func (t *term) sgr(params string) {
	if params == "" {
		params = "0"
	}
	for _, p := range strings.Split(params, ";") {
		n, _ := strconv.Atoi(p)
		switch {
		case n == 0:
			t.fg, t.bold = pFG, false
		case n == 1:
			t.bold = true
		case n == 90:
			t.fg = pDim
		case n == 31 || n == 91:
			t.fg = pRed
		case n == 32 || n == 92:
			t.fg = pGreen
		case n == 33 || n == 93:
			t.fg = pYellow
		case n == 34 || n == 94:
			t.fg = pBlue
		case n == 35 || n == 95:
			t.fg = pMagenta
		case n == 36 || n == 96:
			t.fg = pCyan
		case n == 37 || n == 97 || n == 39:
			t.fg = pFG
		}
	}
}

// ascii transliterates the few non-ASCII symbols MDTI prints; the bitmap
// font only covers ASCII.
var ascii = strings.NewReplacer("→", "->", "·", "|", "—", "-", "–", "-", "…", "...", "°", " deg", "≥", ">=", "≤", "<=",
	"á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ñ", "n", "Á", "A", "É", "E", "Í", "I", "Ó", "O", "Ú", "U", "Ñ", "N")

func (t *term) write(s string) {
	rs := []rune(ascii.Replace(s))
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '\x1b' && i+1 < len(rs) && rs[i+1] == '[':
			j := i + 2
			for j < len(rs) && (rs[j] < '@' || rs[j] > '~') {
				j++
			}
			if j < len(rs) && rs[j] == 'm' {
				t.sgr(string(rs[i+2 : j]))
			}
			i = j
		case r == '\r':
			t.x = 0
		case r == '\n':
			t.newline()
		case r < 32:
		default:
			if r > 126 {
				r = '?' // the bitmap font is ASCII-only
			}
			if t.x >= t.w {
				t.newline()
			}
			fg := t.fg
			if t.bold && fg == pFG {
				fg = pWhite
			}
			t.grid[t.y][t.x] = cell{r: r, fg: fg}
			t.x++
		}
		t.dirty = true
	}
}

const (
	cellW, cellH = 7, 14
	pad          = 12
	barH         = 22
)

func (t *term) render(title string, scale int) *image.Paletted {
	w := t.w*cellW + 2*pad
	h := t.h*cellH + 2*pad + barH
	img := image.NewPaletted(image.Rect(0, 0, w, h), palette)
	for y := 0; y < barH; y++ {
		for x := 0; x < w; x++ {
			img.SetColorIndex(x, y, pBar)
		}
	}
	for i, c := range []uint8{pDotR, pDotY, pDotG} {
		cx, cy := 14+i*16, barH/2
		for y := -5; y <= 5; y++ {
			for x := -5; x <= 5; x++ {
				if x*x+y*y <= 25 {
					img.SetColorIndex(cx+x, cy+y, c)
				}
			}
		}
	}
	d := &font.Drawer{Dst: img, Face: basicfont.Face7x13}
	d.Src = image.NewUniform(palette[pDim])
	d.Dot = fixed.P(w/2-len(title)*cellW/2, barH/2+4)
	d.DrawString(title)
	for y, row := range t.grid {
		for x, c := range row {
			if c.r == 0 || c.r == ' ' {
				continue
			}
			d.Src = image.NewUniform(palette[c.fg])
			d.Dot = fixed.P(pad+x*cellW, barH+pad+y*cellH+11)
			d.DrawString(string(c.r))
		}
	}
	if scale <= 1 {
		return img
	}
	out := image.NewPaletted(image.Rect(0, 0, w*scale, h*scale), palette)
	for y := 0; y < h*scale; y++ {
		for x := 0; x < w*scale; x++ {
			out.Pix[y*out.Stride+x] = img.Pix[(y/scale)*img.Stride+x/scale]
		}
	}
	return out
}

func main() {
	in := flag.String("in", "docs/demo/demo.cast", "asciicast v2 input")
	out := flag.String("out", "docs/demo/demo.gif", "GIF output")
	idle := flag.Float64("idle", 1.5, "cap idle gaps at this many seconds")
	speed := flag.Float64("speed", 1, "playback speed multiplier")
	scale := flag.Int("scale", 2, "integer pixel scale")
	hold := flag.Float64("hold", 6, "seconds to hold the last frame")
	title := flag.String("title", "", "window title (default: the cast title)")
	cols := flag.Int("cols", 0, "override terminal columns (default: the cast width)")
	rows := flag.Int("rows", 0, "override terminal rows (default: the cast height)")
	flag.Parse()
	if err := run(*in, *out, *idle, *speed, *scale, *hold, *title, *cols, *rows); err != nil {
		fmt.Fprintln(os.Stderr, "cast2gif:", err)
		os.Exit(1)
	}
}

func run(in, out string, idle, speed float64, scale int, hold float64, title string, cols, rows int) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	if !sc.Scan() {
		return fmt.Errorf("empty cast")
	}
	var hdr struct {
		Width, Height int
		Title         string
	}
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
		return fmt.Errorf("header: %w", err)
	}
	if title == "" {
		title = hdr.Title
	}
	title = strings.Map(func(r rune) rune {
		if r > 126 {
			return '-'
		}
		return r
	}, ascii.Replace(title))
	if cols > 0 {
		hdr.Width = cols
	}
	if rows > 0 {
		hdr.Height = rows
	}
	type event struct {
		t    float64
		data string
	}
	var evs []event
	for sc.Scan() {
		var raw []any
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil || len(raw) != 3 {
			continue
		}
		t, _ := raw[0].(float64)
		kind, _ := raw[1].(string)
		data, _ := raw[2].(string)
		if kind == "o" {
			evs = append(evs, event{t, data})
		}
	}
	if len(evs) == 0 {
		return fmt.Errorf("no output events")
	}
	term := newTerm(hdr.Width, hdr.Height)
	anim := &gif.GIF{LoopCount: 0}
	for i, e := range evs {
		term.write(e.data)
		gap := hold
		if i+1 < len(evs) {
			gap = min(evs[i+1].t-e.t, idle) / speed
		}
		if gap < 0.04 && i+1 < len(evs) {
			continue // merge bursts into one frame
		}
		anim.Image = append(anim.Image, term.render(title, scale))
		anim.Delay = append(anim.Delay, max(4, int(gap*100)))
	}
	o, err := os.Create(out)
	if err != nil {
		return err
	}
	defer o.Close()
	if err := gif.EncodeAll(o, anim); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d frames\n", out, len(anim.Image))
	return nil
}
