// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package tier1 is the deterministic, zero-LLM-cost anomaly filter (SPEC §6).
// Time is taken from the frames themselves (stream clock), so replays and
// offline evaluation behave exactly like live operation.
package tier1

import (
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// Config holds every Tier-1 threshold (SPEC §6, defaults listed there).
type Config struct {
	WindowSize  int           // frames kept per track
	MinPairDt   time.Duration // below this, speed-derived rules skip the pair
	ResetGap    time.Duration // AIR: a longer gap restarts the window
	Cooldown    time.Duration // dedup window per (entity, type)
	Corroborate time.Duration // other anomalies within this window corroborate

	// D1 kinematic
	VMaxByCategory  map[string]float64 // m/s
	VMaxDefaultAir  float64
	VMaxSea         float64
	SpeedResidual   float64       // m/s floor for |v_impl − gs|
	SpeedResidualPc float64       // fraction of gs
	MaxAccel        float64       // m/s²
	SpeedBaseline   time.Duration // R1/R3 measure speed over at least this span
	TimingSlack     time.Duration // R2 tolerance for aggregator timestamp error
	OscillationN    int
	OscillationWin  time.Duration

	// D2 baro/GNSS
	BaroStepFt      float64
	BaroStepFrames  int
	BaroAbsFt       float64
	BaroRateFtS     float64
	BaroRateDivFpm  float64
	BaroMinBaseline int

	// D3 ghost
	GhostRefs         []string // protected aerodromes (ICAO)
	GhostRadiusNM     float64
	GhostMinAGLFt     float64
	GhostEdgeNM       float64
	GhostRunwayNM     float64
	GhostRunwayMaxAGL float64
	GhostFrames       int
	GhostRecentExpiry time.Duration
	GhostWarmup       time.Duration // tracks first seen this soon after start existed before MDTI started
	CoverageCenter    geo.Point     // ADS-B query circle
	CoverageRadiusNM  float64

	// D4 dark vessel
	DarkMinGap        time.Duration
	DarkGapFactor     float64
	EEZBandNM         float64
	InwardAngleDeg    float64
	CourseAlterDeg    float64
	CourseAlterWin    time.Duration
	CoveragePriorHigh float64
	DarkCheckEvery    time.Duration

	// D5 integrity
	IntegrityDrop  int
	IntegrityFloor int
	IntegrityWin   time.Duration

	// Track eviction
	EvictAfter time.Duration
}

// DefaultConfig returns the SPEC §6 defaults.
func DefaultConfig() Config {
	return Config{
		WindowSize: 20, MinPairDt: 500 * time.Millisecond, ResetGap: 120 * time.Second,
		Cooldown: 120 * time.Second, Corroborate: 60 * time.Second,

		// ADS-B categories are weight classes, not speed classes: A1 includes
		// King Airs and light jets, A2 regional turboprops and business jets.
		// Ceilings are ground speeds, so they include jet-stream tailwinds.
		VMaxByCategory: map[string]float64{
			"A1": 220, "A2": 300, "A3": 360, "A4": 360, "A5": 360, "A6": 700, "A7": 120,
		},
		VMaxDefaultAir: 400, VMaxSea: 25,
		SpeedResidual: 50, SpeedResidualPc: 0.3, MaxAccel: 15,
		SpeedBaseline: 8 * time.Second, TimingSlack: 500 * time.Millisecond,
		OscillationN: 3, OscillationWin: 60 * time.Second,

		BaroStepFt: 500, BaroStepFrames: 3, BaroAbsFt: 2000, BaroRateFtS: 100,
		BaroRateDivFpm: 2000, BaroMinBaseline: 5,

		GhostRefs: []string{"SADM"}, GhostRadiusNM: 15, GhostMinAGLFt: 1500, GhostEdgeNM: 20,
		GhostRunwayNM: 3, GhostRunwayMaxAGL: 3000, GhostFrames: 3,
		GhostRecentExpiry: 30 * time.Minute, GhostWarmup: 20 * time.Second,
		CoverageCenter: geo.Point{Lat: -34.70, Lon: -58.55}, CoverageRadiusNM: 60,

		DarkMinGap: 10 * time.Minute, DarkGapFactor: 6, EEZBandNM: 12, InwardAngleDeg: 60,
		CourseAlterDeg: 30, CourseAlterWin: 10 * time.Minute, CoveragePriorHigh: 0.7,
		DarkCheckEvery: 15 * time.Second,

		IntegrityDrop: 3, IntegrityFloor: 5, IntegrityWin: 30 * time.Second,

		EvictAfter: 30 * time.Minute,
	}
}

// vmax returns the speed ceiling for a frame's category/domain.
func (c *Config) vmax(seaDomain bool, category string) float64 {
	if seaDomain {
		return c.VMaxSea
	}
	if v, ok := c.VMaxByCategory[category]; ok {
		return v
	}
	return c.VMaxDefaultAir
}

// sigmaPos is the 95 % position bound (EPU) for a NACp value, in metres.
func sigmaPos(nacp *int) float64 {
	if nacp == nil {
		return 1852
	}
	switch {
	case *nacp >= 11:
		return 3
	case *nacp == 10:
		return 10
	case *nacp == 9:
		return 30
	case *nacp == 8:
		return 92.6
	case *nacp == 7:
		return 185
	case *nacp == 6:
		return 556
	case *nacp == 5:
		return 926
	default:
		return 1852
	}
}
