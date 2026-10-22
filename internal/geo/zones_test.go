// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package geo

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	zonesOnce sync.Once
	zonesSet  *ZoneSet
	zonesErr  error
)

func loadTestZones(t *testing.T) *ZoneSet {
	t.Helper()
	zonesOnce.Do(func() { zonesSet, zonesErr = LoadZones("../../data/geo/zones.yaml") })
	require.NoError(t, zonesErr)
	return zonesSet
}

func TestZonesContainment(t *testing.T) {
	zs := loadTestZones(t)
	tests := []struct {
		name string
		p    Point
		want string // zone id, "" = none
	}{
		{"offshore Mar del Plata", Point{-38.6, -56.8}, "AR_EEZ"},
		{"Golfo San Jorge offshore", Point{-46.0, -64.5}, "AR_EEZ"},
		{"high seas beyond mile 200 (squid grounds)", Point{-46.0, -58.5}, ""},
		{"Malvinas waters", Point{-51.0, -57.0}, "AR_CLAIMED_MALVINAS"},
		{"South Georgia waters", Point{-54.5, -35.5}, "AR_CLAIMED_SOUTH_GEORGIA_SANDWICH"},
		{"Buenos Aires city (land)", Point{-34.60, -58.38}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := zs.Contains(tc.p)
			if tc.want == "" {
				assert.Nil(t, z)
				return
			}
			require.NotNil(t, z)
			assert.Equal(t, tc.want, z.ID)
		})
	}
}

func TestZonesSignedDistanceAcrossMile200(t *testing.T) {
	zs := loadTestZones(t)
	// Walk east along 46°S from inside the EEZ to the high seas: the signed
	// distance must increase monotonically and change sign exactly once.
	var prev float64
	crossings := 0
	for i, lon := 0, -63.0; lon <= -57.0; i, lon = i+1, lon+0.25 {
		f := zs.Fix(Point{-46.0, lon})
		if i > 0 {
			if (prev < 0) != (f.SignedDistM < 0) {
				crossings++
			}
			assert.Greater(t, f.SignedDistM, prev, "monotonic at lon %v", lon)
		}
		prev = f.SignedDistM
	}
	assert.Equal(t, 1, crossings)
}

func TestZonesInwardBearing(t *testing.T) {
	zs := loadTestZones(t)
	// The Argentina 200 NM line crosses 46°S near 60.75°W ("Milla 201").
	// 8 NM outside it, inward is roughly west.
	f := zs.Fix(Point{-46.0, -60.55})
	require.Nil(t, f.Zone)
	assert.InDelta(t, 8*1852, f.SignedDistM, 3*1852)
	assert.InDelta(t, 270, f.InwardBearingDeg, 60)
	// Inside: inward still points away from the outer limit (westward).
	g := zs.Fix(Point{-46.0, -62.5})
	require.NotNil(t, g.Zone)
	assert.InDelta(t, 270, g.InwardBearingDeg, 60)
}

func TestZonesRejectTamperedFile(t *testing.T) {
	dir := t.TempDir()
	src := "../../data/geo"
	copyFile(t, filepath.Join(src, "zones.yaml"), filepath.Join(dir, "zones.yaml"))
	for _, p := range []string{"boundaries/outer_limits_200nm.geojson", "zones/ar_eez.geojson",
		"zones/overlap_malvinas.geojson", "zones/overlap_south_georgia_sandwich.geojson", "zones/joint_regime_uy_ar.geojson"} {
		copyFile(t, filepath.Join(src, p), filepath.Join(dir, p))
	}
	f, err := os.OpenFile(filepath.Join(dir, "zones/joint_regime_uy_ar.geojson"), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, _ = f.WriteString(" ")
	require.NoError(t, f.Close())

	_, err = LoadZones(filepath.Join(dir, "zones.yaml"))
	assert.ErrorContains(t, err, "sha256 mismatch")
}

func BenchmarkZoneFix(b *testing.B) {
	zs, err := LoadZones("../../data/geo/zones.yaml")
	require.NoError(b, err)
	p := Point{-46.0, -59.8}
	for b.Loop() {
		zs.Fix(p)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	raw, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(to), 0o755))
	require.NoError(t, os.WriteFile(to, raw, 0o644))
}
