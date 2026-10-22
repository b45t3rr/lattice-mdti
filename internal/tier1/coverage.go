// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package tier1

import (
	"math"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// coverageGrid estimates where terrestrial AIS can hear vessels at all, so a
// silence in a never-heard area is not mistaken for a vessel going dark
// (SPEC §6.6, AISCoveragePrior). It keeps, per 0.25° cell, a 24-slot ring of
// the hours in which any vessel was received.
type coverageGrid struct {
	cells map[[2]int]*[24]int64 // slot → hour index (unix hours) when last heard
	first int64                 // first hour observed
}

const cellDeg = 0.25

func newCoverageGrid() *coverageGrid { return &coverageGrid{cells: map[[2]int]*[24]int64{}, first: -1} }

func cellOf(p geo.Point) [2]int {
	return [2]int{int(math.Floor(p.Lat / cellDeg)), int(math.Floor(p.Lon / cellDeg))}
}

// observe records a reception at p and time t.
func (g *coverageGrid) observe(p geo.Point, t time.Time) {
	h := t.Unix() / 3600
	if g.first < 0 || h < g.first {
		g.first = h
	}
	c := cellOf(p)
	ring, ok := g.cells[c]
	if !ok {
		ring = &[24]int64{}
		for i := range ring {
			ring[i] = -1
		}
		g.cells[c] = ring
	}
	ring[h%24] = h
}

// prior returns the fraction of the observed hours (≤ 24) in which nothing was
// received in the 3×3 cell neighbourhood of p (≈ ±20 NM). 1 = never heard.
func (g *coverageGrid) prior(p geo.Point, now time.Time) float64 {
	nowH := now.Unix() / 3600
	if g.first < 0 {
		return 1
	}
	span := min(24, nowH-g.first+1)
	c := cellOf(p)
	silent := 0
	for h := nowH - span + 1; h <= nowH; h++ {
		heard := false
		for dy := -1; dy <= 1 && !heard; dy++ {
			for dx := -1; dx <= 1 && !heard; dx++ {
				if ring, ok := g.cells[[2]int{c[0] + dy, c[1] + dx}]; ok && ring[h%24] == h {
					heard = true
				}
			}
		}
		if !heard {
			silent++
		}
	}
	return float64(silent) / float64(span)
}
