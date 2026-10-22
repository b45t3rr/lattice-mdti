// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package redteam

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/rx/replay"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func render(t *testing.T, file string) *Plan {
	t.Helper()
	s, err := LoadScenario("../../../scenarios/" + file)
	require.NoError(t, err)
	p, err := s.Render(epoch, nil)
	require.NoError(t, err)
	require.NotEmpty(t, p.Frames)
	require.NotEmpty(t, p.Truth)
	for i, f := range p.Frames {
		assert.Equal(t, domain.SourceRedTeam, f.Source)
		if i > 0 {
			assert.False(t, f.ObservedAt.Before(p.Frames[i-1].ObservedAt), "time-ordered")
		}
	}
	return p
}

func maxImpliedSpeed(frames []domain.Frame) float64 {
	best := 0.0
	for i := 1; i < len(frames); i++ {
		a, b := frames[i-1], frames[i]
		dt := b.ObservedAt.Sub(a.ObservedAt).Seconds()
		if dt < 0.5 {
			continue
		}
		best = max(best, geo.Haversine(geo.Point{Lat: a.Lat, Lon: a.Lon}, geo.Point{Lat: b.Lat, Lon: b.Lon})/dt)
	}
	return best
}

func TestKinematicTeleportSignature(t *testing.T) {
	p := render(t, "kinematic.yaml")
	tr := p.Truth[0]
	assert.Equal(t, "KINEMATIC_IMPOSSIBLE", tr.AnomalyType)
	var before, during []domain.Frame
	for _, f := range p.Frames {
		if f.ObservedAt.Before(tr.TStart) {
			before = append(before, f)
		} else {
			during = append(during, f)
		}
	}
	assert.Less(t, maxImpliedSpeed(before), 250.0, "normal phase is a plausible airliner (230 m/s)")
	all := append(before[len(before)-1:], during...)
	assert.Greater(t, maxImpliedSpeed(all), 1372.0, "jump exceeds Mach 4")
}

func TestKinematicOverspeedSignature(t *testing.T) {
	s, err := LoadScenario("../../../scenarios/kinematic.yaml")
	require.NoError(t, err)
	require.NoError(t, s.Params.Decode(&struct{}{}))
	g, err := s.generator()
	require.NoError(t, err)
	kp := g.(*kinematicParams)
	kp.Mode = "overspeed"
	timed, _ := kp.build("x", kp.synthTarget(), newRNG(1))
	last := timed[len(timed)-1].Frame
	assert.InDelta(t, 1400, *last.GroundSpeedMps, 1)
}

func TestBaroGNSSSignature(t *testing.T) {
	p := render(t, "baro_gnss.yaml")
	first, last := p.Frames[0], p.Frames[len(p.Frames)-1]
	d0 := *first.AltGeomFt - *first.AltBaroFt
	d1 := *last.AltGeomFt - *last.AltBaroFt
	assert.InDelta(t, 375, d0, 25, "normal non-ISA offset")
	assert.InDelta(t, 3375, d1, 25, "spoofed offset reaches +3000 ft")
	assert.Equal(t, 9, *first.NACp)
	assert.Equal(t, 4, *last.NACp)
	assert.Equal(t, 3, *last.NIC)
	types := []string{p.Truth[0].AnomalyType, p.Truth[1].AnomalyType}
	assert.ElementsMatch(t, []string{"BARO_GNSS_DIVERGENCE", "NAV_INTEGRITY_DROP"}, types)
}

func TestGhostSignature(t *testing.T) {
	p := render(t, "ghost_sadm.yaml")
	sadm := geo.Point{Lat: -34.6763, Lon: -58.6428}
	first := p.Frames[0]
	assert.InDelta(t, 8*domain.NMToM, geo.Haversine(sadm, geo.Point{Lat: first.Lat, Lon: first.Lon}), 100)
	assert.Empty(t, first.Callsign)
	assert.Nil(t, first.NACp)
	assert.False(t, *first.OnGround)
	assert.Equal(t, 5000.0, *first.AltBaroFt)
	// The racetrack keeps the ghost within the SADM terminal area.
	for _, f := range p.Frames {
		assert.Less(t, geo.Haversine(sadm, geo.Point{Lat: f.Lat, Lon: f.Lon}), 15*domain.NMToM)
	}
	assert.Less(t, maxImpliedSpeed(p.Frames), 120.0, "below the A2 V_MAX: no kinematic side-effect")
}

func TestDarkVesselSignature(t *testing.T) {
	zs, err := geo.LoadZones("../../../data/geo/zones.yaml")
	require.NoError(t, err)
	p := render(t, "dark_vessel_eez.yaml")
	tr := p.Truth[0]
	assert.Equal(t, "AIS_DARK_EEZ", tr.AnomalyType)

	first, last := p.Frames[0], p.Frames[len(p.Frames)-1]
	firstFix := zs.Fix(geo.Point{Lat: first.Lat, Lon: first.Lon})
	assert.Nil(t, firstFix.Zone, "starts on the high seas")
	assert.Less(t, firstFix.SignedDistM, 12*domain.NMToM, "within the mile-200 band")

	lastFix := zs.Fix(geo.Point{Lat: last.Lat, Lon: last.Lon})
	assert.Less(t, lastFix.SignedDistM, 12*domain.NMToM, "goes dark inside the band")
	angle := geo.AngleDiff(*last.TrackDeg, lastFix.InwardBearingDeg)
	assert.Less(t, abs(angle), 60.0, "last course points into the EEZ")

	// Dead-reckon 45 minutes of silence: the vessel is inside the EEZ.
	dr := geo.Destination(geo.Point{Lat: last.Lat, Lon: last.Lon}, *last.TrackDeg, *last.GroundSpeedMps*45*60)
	z := zs.Contains(dr)
	require.NotNil(t, z)
	assert.Equal(t, "AR_EEZ", z.ID)

	assert.Equal(t, last.ObservedAt.Add(10*time.Minute).Round(time.Second), tr.TStart.Round(time.Second))
	assert.Equal(t, 30, *last.ShipType)
}

func TestRenderIsDeterministic(t *testing.T) {
	for _, f := range []string{"kinematic.yaml", "baro_gnss.yaml", "ghost_sadm.yaml", "dark_vessel_eez.yaml"} {
		t.Run(f, func(t *testing.T) {
			a, b := encode(t, render(t, f)), encode(t, render(t, f))
			assert.Equal(t, sha256.Sum256(a), sha256.Sum256(b), "byte-identical for the same seed")
		})
	}
}

func TestDifferentSeedsDiffer(t *testing.T) {
	s, err := LoadScenario("../../../scenarios/ghost_sadm.yaml")
	require.NoError(t, err)
	a, _ := s.Render(epoch, nil)
	s.Seed++
	b, _ := s.Render(epoch, nil)
	assert.NotEqual(t, sha256.Sum256(encode(t, a)), sha256.Sum256(encode(t, b)))
}

func TestHijackUsesLiveTrack(t *testing.T) {
	s, err := LoadScenario("../../../scenarios/kinematic.yaml")
	require.NoError(t, err)
	s.StartAfter = 0
	s.Target.HijackWait = time.Second

	live := make(chan domain.Frame, 8)
	live <- domain.Frame{EntityKey: "adsb:e0b244", Domain: domain.DomainAir, OnGround: domain.Ptr(true)} // rejected: on ground
	stale := domain.Frame{EntityKey: "adsb:e0aaaa", Domain: domain.DomainAir, Lat: -34.9, Lon: -58.3, ObservedAt: time.Now().Add(-time.Minute),
		OnGround: domain.Ptr(false), AltBaroFt: domain.Ptr(32100.0), GroundSpeedMps: domain.Ptr(240.0), TrackDeg: domain.Ptr(200.0)}
	live <- stale // rejected: too old to extrapolate from
	live <- domain.Frame{EntityKey: "adsb:e0658e", Domain: domain.DomainAir, Lat: -34.9, Lon: -58.3, ObservedAt: time.Now(),
		OnGround: domain.Ptr(false), AltBaroFt: domain.Ptr(32100.0), AltGeomFt: domain.Ptr(32500.0),
		GroundSpeedMps: domain.Ptr(240.0), TrackDeg: domain.Ptr(200.0), Callsign: "ARG1350"}

	out := make(chan domain.Frame, 256)
	var truth bytes.Buffer
	r := &Runner{Scenario: s, Log: slog.New(slog.DiscardHandler), Truth: &truth}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Speed the attack up: only check the first frames.
	go func() { _ = r.Run(ctx, out, live) }()
	f := <-out
	assert.Equal(t, "adsb:e0658e", f.EntityKey, "re-broadcasts the live ICAO")
	assert.Equal(t, "ARG1350", f.Callsign)
	assert.Contains(t, truth.String(), `"entity_key":"adsb:e0658e"`)
}

func TestLoadScenarioRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := dir + "/s.yaml"
		require.NoError(t, writeFile(p, body))
		return p
	}
	_, err := LoadScenario(write("name: x\nattack: jamming\n"))
	assert.ErrorContains(t, err, "unknown attack")
	_, err = LoadScenario(write("name: x\nattack: ghost\ntarget: {mode: hijack}\n"))
	assert.ErrorContains(t, err, "always synthesizes")
	_, err = LoadScenario(write("name: x\nattack: ghost\nparams: {spawn: {ref: KJFK}}\n"))
	assert.ErrorContains(t, err, "spawn.ref")
}

func encode(t *testing.T, p *Plan) []byte {
	t.Helper()
	var buf bytes.Buffer
	rec := replay.NewRecorder(&buf)
	for _, f := range p.Frames {
		require.NoError(t, rec.Write(f))
	}
	require.NoError(t, rec.Flush())
	require.NoError(t, WriteTruth(&buf, p.Truth))
	return buf.Bytes()
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
