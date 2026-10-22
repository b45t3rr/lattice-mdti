// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package synth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/tier1"
)

// The background traffic is the control group: 30 minutes of it must not
// raise a single Tier-1 anomaly.
func TestTrafficIsBenignForTier1(t *testing.T) {
	zones, err := geo.LoadZones("../../../data/geo/zones.yaml")
	require.NoError(t, err)
	aero, err := geo.LoadAerodromes("../../../data/geo/aerodromes.json")
	require.NoError(t, err)
	center := geo.Point{Lat: -34.70, Lon: -58.55}
	cfg := tier1.DefaultConfig()
	cfg.CoverageCenter, cfg.CoverageRadiusNM = center, 60
	eng := tier1.New(cfg, zones, aero)

	tr := &Traffic{Center: center, RadiusNM: 60, Aircraft: 14, Seed: 42,
		Avoid: []geo.Point{aero["SADM"].Point()}, AvoidNM: 20}
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tr.Start(start)
	keys := map[string]bool{}
	var air, sea int
	for i := range 1800 {
		now := start.Add(time.Duration(i) * time.Second)
		for _, f := range tr.Step(now) {
			require.NoError(t, f.Validate(now))
			keys[f.EntityKey] = true
			if f.Domain == domain.DomainAir {
				air++
				assert.LessOrEqual(t, geo.Haversine(center, geo.Point{Lat: f.Lat, Lon: f.Lon}), 60*domain.NMToM+1)
			} else {
				sea++
			}
			for _, ev := range eng.Process(f.Strip()) {
				t.Errorf("benign traffic raised %s on %s (%s)", ev.Type, ev.EntityKey, ev.Rule)
			}
		}
		for _, ev := range eng.Advance(now) {
			t.Errorf("benign traffic raised %s on %s (%s)", ev.Type, ev.EntityKey, ev.Rule)
		}
	}
	assert.Equal(t, 14*1800, air)
	assert.Greater(t, sea, 14*170)
	assert.Greater(t, len(keys), 14+14, "aircraft that leave coverage are replaced")
}

func TestTrafficIsDeterministic(t *testing.T) {
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	run := func() []domain.Frame {
		tr := &Traffic{Center: geo.Point{Lat: -34.70, Lon: -58.55}, RadiusNM: 60, Aircraft: 5, Seed: 7}
		tr.Start(start)
		return tr.Step(start.Add(time.Second))
	}
	assert.Equal(t, run(), run())
}
