// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package tier1

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/rx/redteam"
)

var (
	t0       = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	geoOnce  sync.Once
	zonesSet *geo.ZoneSet
	aeros    geo.Aerodromes
)

func newEngine(t *testing.T, mut ...func(*Config)) *Engine {
	t.Helper()
	geoOnce.Do(func() {
		var err error
		zonesSet, err = geo.LoadZones("../../data/geo/zones.yaml")
		require.NoError(t, err)
		aeros, err = geo.LoadAerodromes("../../data/geo/aerodromes.json")
		require.NoError(t, err)
	})
	c := DefaultConfig()
	c.GhostWarmup = 0
	for _, m := range mut {
		m(&c)
	}
	return New(c, zonesSet, aeros)
}

// airliner returns a cruising A3 frame stream generator: 230 m/s on 315°,
// FL240 with a +375 ft non-ISA offset, NACp 9, 1 Hz.
type flight struct {
	key  string
	pos  geo.Point
	at   time.Time
	gs   float64
	trk  float64
	baro float64
	geom float64
	nacp int
	nic  int
	call string
}

func newFlight(key string) *flight {
	return &flight{key: key, pos: geo.Point{Lat: -35.1, Lon: -58.1}, at: t0, gs: 230, trk: 315,
		baro: 24000, geom: 24375, nacp: 9, nic: 8, call: "ARG1234"}
}

func (f *flight) frame() domain.Frame {
	return domain.Frame{
		EntityKey: f.key, Domain: domain.DomainAir, ObservedAt: f.at, Lat: f.pos.Lat, Lon: f.pos.Lon,
		GroundSpeedMps: domain.Ptr(f.gs), TrackDeg: domain.Ptr(f.trk), AltBaroFt: domain.Ptr(f.baro),
		AltGeomFt: domain.Ptr(f.geom), OnGround: domain.Ptr(false), NACp: domain.Ptr(f.nacp), NIC: domain.Ptr(f.nic),
		Callsign: f.call, Category: "A3", Squawk: "4521",
	}
}

func (f *flight) step(dt time.Duration) {
	f.pos = geo.Destination(f.pos, f.trk, f.gs*dt.Seconds())
	f.at = f.at.Add(dt)
}

func run(e *Engine, f *flight, n int, dt time.Duration) []domain.AnomalyEvent {
	var evs []domain.AnomalyEvent
	for range n {
		evs = append(evs, e.Process(f.frame())...)
		f.step(dt)
	}
	return evs
}

func rules(evs []domain.AnomalyEvent) []string {
	var r []string
	for _, ev := range evs {
		r = append(r, ev.Rule)
	}
	return r
}

func TestNormalAirlinerIsQuiet(t *testing.T) {
	e := newEngine(t)
	evs := run(e, newFlight("adsb:aaaaaa"), 120, time.Second)
	assert.Empty(t, rules(evs))
}

// Aggregator timestamps carry ±0.5 s of error (observed live: pairwise speeds
// of 80–338 m/s for an A321 at 263 m/s). That must not trip any rule.
func TestTimestampJitterIsQuiet(t *testing.T) {
	e := newEngine(t)
	f := newFlight("adsb:aaaaaa")
	f.gs = 263
	jit := []time.Duration{-500, 400, -300, 500, 0, -450, 350, -200}
	for i := range 60 {
		fr := f.frame()
		fr.ObservedAt = f.at.Add(jit[i%len(jit)] * time.Millisecond)
		assert.Empty(t, e.Process(fr), "frame %d", i)
		f.step(2 * time.Second)
	}
}

func TestD1Teleport(t *testing.T) {
	e := newEngine(t)
	f := newFlight("adsb:aaaaaa")
	run(e, f, 10, time.Second)
	f.pos = geo.Destination(f.pos, 45, 60*domain.NMToM)
	f.at = f.at.Add(time.Second)
	evs := e.Process(f.frame())
	require.Len(t, evs, 1)
	assert.Equal(t, domain.AnomalyKinematicImpossible, evs[0].Type)
	assert.Equal(t, "D1.R2 teleport", evs[0].Rule)
	assert.Equal(t, 4, evs[0].Severity)
	assert.True(t, evs[0].Escalate)
	assert.Greater(t, evs[0].Metrics["implied_speed_mps"], 1372.0)
}

func TestD1TeleportWithSubHalfSecondInterleave(t *testing.T) {
	// A hijacked ICAO: the real report and the injected one arrive 0.3 s apart.
	e := newEngine(t)
	f := newFlight("adsb:aaaaaa")
	run(e, f, 5, time.Second)
	fake := f.frame()
	fake.ObservedAt = f.at.Add(-700 * time.Millisecond)
	p := geo.Destination(f.pos, 0, 60*domain.NMToM)
	fake.Lat, fake.Lon = p.Lat, p.Lon
	evs := e.Process(fake)
	require.NotEmpty(t, evs)
	assert.Equal(t, "D1.R2 teleport", evs[0].Rule)
}

func TestD1SmallJitterIsNotTeleport(t *testing.T) {
	e := newEngine(t)
	f := newFlight("adsb:aaaaaa")
	run(e, f, 5, time.Second)
	last := e.tracks["mdti-adsb-aaaaaa"].frames[4]
	j := f.frame()
	j.ObservedAt = last.ObservedAt.Add(100 * time.Millisecond)
	p := geo.Destination(pt(last), 0, 40) // 40 m: inside 2σ for NACp 9
	j.Lat, j.Lon = p.Lat, p.Lon
	assert.Empty(t, e.Process(j))
}

func TestD1Oscillation(t *testing.T) {
	e := newEngine(t, func(c *Config) { c.Cooldown = time.Hour })
	f := newFlight("adsb:aaaaaa")
	run(e, f, 5, time.Second)
	// 0.5 NM jumps: impossible (sev 3) but below 3×V_MAX, so R5 (sev 4) escalates past the cooldown.
	real, far := f.pos, geo.Destination(f.pos, 0, 0.5*domain.NMToM)
	var all []domain.AnomalyEvent
	for i := range 6 {
		f.at = f.at.Add(time.Second)
		if i%2 == 0 {
			f.pos = far
		} else {
			f.pos = real
		}
		all = append(all, e.Process(f.frame())...)
	}
	assert.Contains(t, rules(all), "D1.R2 teleport")
	assert.Contains(t, rules(all), "D1.R5 oscillation", "R5 escalates past the cooldown")
}

func TestD1Overspeed(t *testing.T) {
	e := newEngine(t)
	f := newFlight("adsb:aaaaaa")
	f.gs = 420 // above the 360 m/s airliner ceiling, but no single-pair teleport
	evs := run(e, f, 12, time.Second)
	assert.Contains(t, rules(evs), "D1.R1 overspeed")
}

func TestD1SeaVesselOverspeed(t *testing.T) {
	e := newEngine(t)
	at := t0
	p := geo.Point{Lat: -38.6, Lon: -56.8}
	var evs []domain.AnomalyEvent
	for range 6 {
		evs = append(evs, e.Process(domain.Frame{EntityKey: "ais:701000001", Domain: domain.DomainSea, ObservedAt: at,
			Lat: p.Lat, Lon: p.Lon, GroundSpeedMps: domain.Ptr(5.0), TrackDeg: domain.Ptr(90.0), AISClass: "A"})...)
		p = geo.Destination(p, 90, 400) // 40 m/s over 10 s
		at = at.Add(10 * time.Second)
	}
	assert.Contains(t, rules(evs), "D1.R1 overspeed")
}

func TestD2BaroGNSS(t *testing.T) {
	t.Run("step against baseline", func(t *testing.T) {
		e := newEngine(t)
		f := newFlight("adsb:aaaaaa")
		run(e, f, 10, time.Second)
		f.geom += 800 // sudden +800 ft, sustained
		evs := run(e, f, 3, time.Second)
		require.NotEmpty(t, evs)
		assert.Equal(t, "D2.R1 step", evs[len(evs)-1].Rule)
	})
	t.Run("absolute", func(t *testing.T) {
		e := newEngine(t)
		f := newFlight("adsb:aaaaaa")
		f.geom = f.baro + 2500
		evs := run(e, f, 1, time.Second)
		require.Len(t, evs, 1)
		assert.Equal(t, "D2.R2 absolute", evs[0].Rule)
	})
	t.Run("large but stable non-ISA offset is benign", func(t *testing.T) {
		e := newEngine(t)
		f := newFlight("adsb:aaaaaa")
		f.baro, f.geom = 30475, 31450 // +975 ft observed live at FL305 over AMBA
		assert.Empty(t, run(e, f, 60, time.Second))
	})
	t.Run("rate", func(t *testing.T) {
		e := newEngine(t, func(c *Config) { c.BaroStepFt = 1e9 })
		f := newFlight("adsb:aaaaaa")
		run(e, f, 3, time.Second)
		var evs []domain.AnomalyEvent
		for range 3 {
			f.geom += 150
			evs = append(evs, run(e, f, 1, time.Second)...)
		}
		assert.Contains(t, rules(evs), "D2.R3 rate")
	})
	t.Run("integrity drop raises severity", func(t *testing.T) {
		e := newEngine(t)
		f := newFlight("adsb:aaaaaa")
		run(e, f, 10, time.Second)
		f.nacp, f.nic = 4, 3
		f.geom += 800
		evs := run(e, f, 3, time.Second)
		require.NotEmpty(t, evs)
		last := evs[len(evs)-1]
		assert.Equal(t, domain.AnomalyBaroGNSSDivergence, last.Type)
		assert.Equal(t, 4, last.Severity)
		assert.Contains(t, last.Corroborating, domain.AnomalyNavIntegrityDrop)
	})
}

func TestD5IntegrityDrop(t *testing.T) {
	e := newEngine(t)
	f := newFlight("adsb:aaaaaa")
	run(e, f, 5, time.Second)
	f.nacp = 5
	evs := run(e, f, 1, time.Second)
	require.Len(t, evs, 1)
	assert.Equal(t, domain.AnomalyNavIntegrityDrop, evs[0].Type)
	assert.Equal(t, 2, evs[0].Severity)
	assert.False(t, evs[0].Escalate, "a lone integrity drop is not escalated")

	e2 := newEngine(t)
	g := newFlight("adsb:bbbbbb")
	run(e2, g, 5, time.Second)
	g.nacp = 7 // drop of 2 to a value above the floor
	assert.Empty(t, run(e2, g, 3, time.Second))
}

func ghost(key string, at time.Time, from geo.Point) *flight {
	f := newFlight(key)
	f.at, f.pos = at, from
	f.gs, f.trk, f.baro, f.geom, f.call = 108, 120, 5000, 5150, ""
	return f
}

func TestD3Ghost(t *testing.T) {
	sadm := geo.Point{Lat: -34.6763, Lon: -58.6428}
	spawn := geo.Destination(sadm, 300, 8*domain.NMToM)

	t.Run("mid-coverage spawn near SADM", func(t *testing.T) {
		e := newEngine(t)
		evs := run(e, ghost("adsb:e3f00d", t0, spawn), 5, time.Second)
		require.Len(t, evs, 1)
		assert.Equal(t, domain.AnomalyGhostTrack, evs[0].Type)
		assert.Equal(t, 3, evs[0].Severity, "base 2 + empty callsign")
	})
	t.Run("aircraft present at start-up is not a ghost", func(t *testing.T) {
		e := newEngine(t, func(c *Config) { c.GhostWarmup = 20 * time.Second })
		evs := run(e, ghost("adsb:e3f00d", t0, spawn), 5, time.Second)
		assert.Empty(t, evs)
	})
	t.Run("warm-up counts from the first aircraft, not from lagging AIS", func(t *testing.T) {
		e := newEngine(t, func(c *Config) { c.GhostWarmup = 20 * time.Second })
		e.Process(vessel(t0.Add(-2*time.Minute), geo.Point{Lat: -35.18, Lon: -55.5}, 0)) // AIS report 2 min old
		run(e, newFlight("adsb:bbbbbb"), 1, time.Second)                                 // first aircraft at t0
		assert.Empty(t, run(e, ghost("adsb:e3f00d", t0.Add(time.Second), spawn), 5, time.Second))
	})
	t.Run("entering from the coverage edge", func(t *testing.T) {
		e := newEngine(t, func(c *Config) { c.CoverageRadiusNM = 20 })
		assert.Empty(t, run(e, ghost("adsb:e3f00d", t0, spawn), 5, time.Second))
	})
	t.Run("departure from Morón", func(t *testing.T) {
		e := newEngine(t)
		g := ghost("adsb:e3f00d", t0, geo.Destination(sadm, 10, 1500))
		g.baro, g.geom = 1800, 1950
		assert.Empty(t, run(e, g, 5, time.Second))
	})
	t.Run("far from protected aerodromes", func(t *testing.T) {
		e := newEngine(t)
		assert.Empty(t, run(e, ghost("adsb:e3f00d", t0, geo.Point{Lat: -35.3, Lon: -58.1}), 5, time.Second))
	})
	t.Run("recently expired track reappearing", func(t *testing.T) {
		e := newEngine(t, func(c *Config) { c.EvictAfter = time.Minute })
		run(e, ghost("adsb:e3f00d", t0, geo.Point{Lat: -35.3, Lon: -58.1}), 3, time.Second)
		e.Advance(t0.Add(3 * time.Minute)) // evicted
		evs := run(e, ghost("adsb:e3f00d", t0.Add(4*time.Minute), spawn), 5, time.Second)
		assert.Empty(t, evs)
	})
}

func vessel(at time.Time, p geo.Point, cog float64) domain.Frame {
	return domain.Frame{EntityKey: "ais:200000001", Domain: domain.DomainSea, ObservedAt: at, Lat: p.Lat, Lon: p.Lon,
		GroundSpeedMps: domain.Ptr(9 * domain.KnotToMps), TrackDeg: domain.Ptr(cog), AISClass: "A", ShipType: domain.Ptr(30)}
}

func TestD4Dark(t *testing.T) {
	t.Run("silent inside the band heading in", func(t *testing.T) {
		e := newEngine(t)
		p := geo.Point{Lat: -46.40, Lon: -60.80}
		for i := range 10 {
			e.Process(vessel(t0.Add(time.Duration(i)*10*time.Second), p, 285))
		}
		assert.Empty(t, e.Advance(t0.Add(5*time.Minute)), "too early")
		var evs []domain.AnomalyEvent
		for m := 5; m <= 15; m++ {
			evs = append(evs, e.Advance(t0.Add(time.Duration(m)*time.Minute))...)
		}
		require.Len(t, evs, 1)
		ev := evs[0]
		assert.Equal(t, domain.AnomalyAISDarkEEZ, ev.Type)
		assert.Equal(t, 4, ev.Severity, "base 3 + fishing")
		require.NotNil(t, ev.Geo.SignedDistToEEZNM)
		assert.Less(t, *ev.Geo.SignedDistToEEZNM, 12.0)
		assert.Empty(t, e.Advance(t0.Add(30*time.Minute)), "one event per dark episode")
	})
	t.Run("the vessel's own silence does not lower the coverage prior", func(t *testing.T) {
		e := newEngine(t)
		start := time.Date(2026, 1, 1, 12, 50, 0, 0, time.UTC) // silence crosses the 13:00 hour boundary
		p := geo.Point{Lat: -46.40, Lon: -60.80}
		for i := range 10 {
			e.Process(vessel(start.Add(time.Duration(i)*10*time.Second), p, 285))
		}
		var evs []domain.AnomalyEvent
		for m := 1; m <= 20; m++ {
			evs = append(evs, e.Advance(start.Add(time.Duration(m)*time.Minute))...)
		}
		require.Len(t, evs, 1)
		assert.Equal(t, 0.0, evs[0].Metrics["coverage_prior"])
		assert.Equal(t, 0.0, *evs[0].Geo.AISCoveragePrior)
	})
	t.Run("heading away is benign", func(t *testing.T) {
		e := newEngine(t)
		p := geo.Point{Lat: -46.40, Lon: -60.80}
		for i := range 5 {
			e.Process(vessel(t0.Add(time.Duration(i)*10*time.Second), p, 90))
		}
		assert.Empty(t, e.Advance(t0.Add(20*time.Minute)))
	})
	t.Run("coastal vessel far from mile 200", func(t *testing.T) {
		e := newEngine(t)
		for i := range 5 {
			e.Process(vessel(t0.Add(time.Duration(i)*10*time.Second), geo.Point{Lat: -38.05, Lon: -57.53}, 290))
		}
		assert.Empty(t, e.Advance(t0.Add(20*time.Minute)))
	})
	t.Run("anchored vessel uses the 3-minute interval", func(t *testing.T) {
		f := vessel(t0, geo.Point{}, 0)
		f.NavStatus = domain.Ptr(1)
		assert.Equal(t, 180*time.Second, expectedInterval(f))
		f.AISClass = "B"
		assert.Equal(t, 30*time.Second, expectedInterval(f))
	})
}

func TestCoveragePrior(t *testing.T) {
	g := newCoverageGrid()
	p := geo.Point{Lat: -35.18, Lon: -55.5}
	assert.Equal(t, 1.0, g.prior(p, t0), "nothing observed yet")
	g.observe(p, t0)
	assert.Equal(t, 0.0, g.prior(p, t0.Add(10*time.Minute)))
	assert.Equal(t, 0.0, g.prior(geo.Destination(p, 0, 15*domain.NMToM), t0), "neighbouring cell counts")
	assert.Equal(t, 1.0, g.prior(geo.Point{Lat: -50, Lon: -60}, t0))
	assert.InDelta(t, 0.5, g.prior(p, t0.Add(time.Hour)), 1e-9, "heard in 1 of 2 observed hours")
}

func TestCooldown(t *testing.T) {
	e := newEngine(t)
	f := newFlight("adsb:aaaaaa")
	f.geom = f.baro + 2500
	evs := run(e, f, 60, time.Second)
	assert.Len(t, evs, 1, "repeats suppressed for 120 s")
	evs = run(e, f, 70, time.Second)
	assert.Len(t, evs, 1, "re-emitted after the cooldown")
}

func TestOutOfOrderIgnored(t *testing.T) {
	e := newEngine(t)
	f := newFlight("adsb:aaaaaa")
	run(e, f, 3, time.Second)
	old := f.frame()
	old.ObservedAt = t0
	assert.Empty(t, e.Process(old))
	assert.Equal(t, uint64(1), e.Ignored)
}

func TestSourceNeverReachesEvents(t *testing.T) {
	e := newEngine(t)
	f := newFlight("adsb:aaaaaa")
	run(e, f, 3, time.Second)
	fr := f.frame()
	fr.Source = domain.SourceRedTeam
	fr.ObservedAt = f.at
	p := geo.Destination(f.pos, 0, 60*domain.NMToM)
	fr.Lat, fr.Lon = p.Lat, p.Lon
	evs := e.Process(fr)
	require.NotEmpty(t, evs)
	for _, w := range evs[0].Window {
		assert.Empty(t, w.Source)
	}
}

// Regression: every scenario, several seeds, recall 1 and no false positives.
func TestScenarioRegression(t *testing.T) {
	files := []string{"kinematic.yaml", "baro_gnss.yaml", "ghost_sadm.yaml", "dark_vessel_eez.yaml"}
	for _, seed := range []int64{1, 7, 42, 1234} {
		e := newEngine(t)
		var frames []domain.Frame
		var truth []Truth
		for _, name := range files {
			s, err := redteam.LoadScenario("../../scenarios/" + name)
			require.NoError(t, err)
			s.Seed = seed
			p, err := s.Render(t0, nil)
			require.NoError(t, err)
			frames = append(frames, p.Frames...)
			for _, tr := range p.Truth {
				truth = append(truth, Truth{Scenario: tr.Scenario, EntityKey: tr.EntityKey,
					AnomalyType: tr.AnomalyType, TStart: tr.TStart, TEnd: tr.TEnd})
			}
		}
		sortFrames(frames)
		var evs []domain.AnomalyEvent
		for _, f := range frames {
			evs = append(evs, e.Process(f)...)
		}
		last := frames[len(frames)-1].ObservedAt
		for at := last; at.Before(last.Add(30 * time.Minute)); at = at.Add(15 * time.Second) {
			evs = append(evs, e.Advance(at)...)
		}
		rep := Score(evs, truth)
		assert.Equal(t, 1.0, rep.Recall, "seed %d", seed)
		assert.Empty(t, rep.FalsePositives, "seed %d", seed)
	}
}

func sortFrames(fs []domain.Frame) {
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && fs[j].ObservedAt.Before(fs[j-1].ObservedAt); j-- {
			fs[j], fs[j-1] = fs[j-1], fs[j]
		}
	}
}
