// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package geo

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHaversine(t *testing.T) {
	tests := []struct {
		name string
		a, b Point
		want float64 // metres
		tol  float64
	}{
		{"one degree of longitude at equator", Point{0, 0}, Point{0, 1}, 2 * math.Pi * EarthRadiusM / 360, 1e-6},
		{"one degree of latitude", Point{0, 0}, Point{1, 0}, 2 * math.Pi * EarthRadiusM / 360, 1e-6},
		{"same point", Point{-34.6763, -58.6428}, Point{-34.6763, -58.6428}, 0, 1e-9},
		// Antipodes: half the circumference.
		{"antipodal", Point{0, 0}, Point{0, 180}, math.Pi * EarthRadiusM, 1e-3},
		// SADM → SAEZ, cross-checked with an independent Vincenty/haversine calculator (≈ 18.97 km).
		{"SADM to SAEZ", Point{-34.6763, -58.6428}, Point{-34.8222, -58.5358}, 18970, 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, Haversine(tc.a, tc.b), tc.tol)
			assert.InDelta(t, Haversine(tc.a, tc.b), Haversine(tc.b, tc.a), 1e-9, "symmetry")
		})
	}
}

func TestBearing(t *testing.T) {
	assert.InDelta(t, 0, Bearing(Point{0, 0}, Point{1, 0}), 1e-9)
	assert.InDelta(t, 90, Bearing(Point{0, 0}, Point{0, 1}), 1e-9)
	assert.InDelta(t, 180, Bearing(Point{1, 0}, Point{0, 0}), 1e-9)
	assert.InDelta(t, 270, Bearing(Point{0, 1}, Point{0, 0}), 1e-9)
}

func TestDestinationRoundTrip(t *testing.T) {
	start := Point{-46.0, -59.8}
	for _, brg := range []float64{0, 45, 90, 135, 180, 225, 270, 315, 359.9} {
		for _, d := range []float64{10, 1852, 50_000, 370_400} {
			end := Destination(start, brg, d)
			assert.InDelta(t, d, Haversine(start, end), d*1e-9+1e-6, "distance brg=%v d=%v", brg, d)
			if d > 1 {
				assert.InDelta(t, 0, AngleDiff(brg, Bearing(start, end)), 1e-6, "bearing brg=%v d=%v", brg, d)
			}
		}
	}
}

func TestDestinationWrapsAntimeridian(t *testing.T) {
	p := Destination(Point{0, 179.9}, 90, 50_000)
	assert.Less(t, p.Lon, -179.0)
}

func TestAngleDiff(t *testing.T) {
	tests := []struct{ a, b, want float64 }{
		{10, 20, 10}, {20, 10, -10}, {350, 10, 20}, {10, 350, -20}, {0, 180, 180}, {180, 0, 180}, {90, 90, 0},
	}
	for _, tc := range tests {
		assert.InDelta(t, tc.want, AngleDiff(tc.a, tc.b), 1e-9, "%v→%v", tc.a, tc.b)
	}
}

func TestENUMatchesHaversineLocally(t *testing.T) {
	o := Point{-34.6763, -58.6428}
	p := Destination(o, 37, 40_000)
	e, n := ENU(o, p)
	assert.InDelta(t, Haversine(o, p), math.Hypot(e, n), 40_000*0.005)
}

func TestVelocityENU(t *testing.T) {
	e, n := VelocityENU(100, 90)
	assert.InDelta(t, 100, e, 1e-9)
	assert.InDelta(t, 0, n, 1e-9)
}
