// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package agent simulates MDTI's taskable patrol assets (SPEC §8.4–8.6).
package agent

import (
	"math"

	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// State is an asset's kinematic state.
type State struct {
	Pos      geo.Point
	Heading  float64 // true, degrees
	SpeedMps float64
}

// MaxTurnRate is a standard-rate turn (3°/s), so tracks look plausible.
const MaxTurnRate = 3.0

// StepToward advances dt seconds toward target at speed, turning at most
// MaxTurnRate·dt. It never overshoots the target.
func StepToward(s State, target geo.Point, speed, dt float64) State {
	dist := geo.Haversine(s.Pos, target)
	if dist < 1 {
		s.SpeedMps = 0
		return s
	}
	want := geo.Bearing(s.Pos, target)
	turn := geo.AngleDiff(s.Heading, want)
	limit := MaxTurnRate * dt
	turn = math.Max(-limit, math.Min(limit, turn))
	s.Heading = geo.NormalizeDeg(s.Heading + turn)
	step := math.Min(speed*dt, dist)
	s.Pos = geo.Destination(s.Pos, s.Heading, step)
	s.SpeedMps = speed
	return s
}

// OrbitStep advances dt seconds around center at radius (clockwise as seen
// from above). If the asset is off the circle it spirals onto it.
func OrbitStep(s State, center geo.Point, radius, speed, dt float64) State {
	r := geo.Haversine(center, s.Pos)
	bearing := geo.Bearing(center, s.Pos)
	// Ease the radius towards the orbit radius, then advance along the circle.
	r += (radius - r) * math.Min(1, dt/10)
	bearing = geo.NormalizeDeg(bearing + (speed*dt/math.Max(r, 1))*180/math.Pi)
	next := geo.Destination(center, bearing, r)
	s.Heading = geo.NormalizeDeg(bearing + 90)
	s.Pos = next
	s.SpeedMps = speed
	return s
}
