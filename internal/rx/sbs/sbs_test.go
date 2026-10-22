// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package sbs

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/tier1"
)

// msg builds a synthetic SBS line: type, hex and the fields that matter.
func msg(typ int, hex, callsign, alt, gs, track, lat, lon, vrate, squawk, ground string) string {
	return fmt.Sprintf("MSG,%d,1,1,%s,1,2026/10/22,12:00:00.000,2026/10/22,12:00:00.000,%s,%s,%s,%s,%s,%s,%s,%s,0,0,0,%s",
		typ, hex, callsign, alt, gs, track, lat, lon, vrate, squawk, ground)
}

func collect(s *Source, lines []string, now time.Time) []domain.Frame {
	out := make(chan domain.Frame, 64)
	for i, l := range lines {
		s.HandleLine(l, now.Add(time.Duration(i)*time.Second), out)
	}
	close(out)
	var fs []domain.Frame
	for f := range out {
		fs = append(fs, f)
	}
	return fs
}

func newSource(t *testing.T) *Source {
	t.Helper()
	s, err := New("127.0.0.1:30003", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	return s
}

func TestPartialUpdatesAreMerged(t *testing.T) {
	s := newSource(t)
	fs := collect(s, []string{
		msg(1, "E0C0DE", "ARG1234 ", "", "", "", "", "", "", "", ""),
		msg(4, "E0C0DE", "", "", "450", "315", "", "", "0", "", ""),
		msg(6, "E0C0DE", "", "24000", "", "", "", "", "", "4521", "0"),
		msg(3, "E0C0DE", "", "24000", "", "", "-35.1", "-58.1", "", "", "0"),
	}, time.Now())
	require.Len(t, fs, 1, "only the position line emits a frame")
	f := fs[0]
	assert.Equal(t, "adsb:e0c0de", f.EntityKey)
	assert.Equal(t, domain.SourceSBS, f.Source)
	assert.Equal(t, "ARG1234", f.Callsign)
	assert.Equal(t, "4521", f.Squawk)
	assert.InDelta(t, 24000, *f.AltBaroFt, 1e-9)
	assert.InDelta(t, 450*domain.KnotToMps, *f.GroundSpeedMps, 1e-9)
	assert.InDelta(t, 315, *f.TrackDeg, 1e-9)
	assert.False(t, *f.OnGround)
	assert.Nil(t, f.AltGeomFt, "SBS has no geometric altitude: not reported, never zero-filled")
	assert.Nil(t, f.NACp)
}

func TestInvalidLinesAreCounted(t *testing.T) {
	s := newSource(t)
	fs := collect(s, []string{"", "garbage", "MSG,3,1,1", msg(3, "XYZ123", "", "1000", "", "", "-34", "-58", "", "", "0"),
		msg(3, "E0C0DE", "", "1000", "", "", "0", "0", "", "", "0")}, time.Now())
	assert.Empty(t, fs)
	assert.Equal(t, uint64(4), s.Invalid.Load(), "short line, bad hex and a null-island position")
}

// An SBS feed with an impossible jump must reach Tier 1 as KINEMATIC_IMPOSSIBLE.
func TestTeleportOverSBSIsDetected(t *testing.T) {
	zones, err := geo.LoadZones("../../../data/geo/zones.yaml")
	require.NoError(t, err)
	aero, err := geo.LoadAerodromes("../../../data/geo/aerodromes.json")
	require.NoError(t, err)
	cfg := tier1.DefaultConfig()
	cfg.CoverageCenter, cfg.CoverageRadiusNM = geo.Point{Lat: -34.70, Lon: -58.55}, 60
	eng := tier1.New(cfg, zones, aero)

	s := newSource(t)
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	p := geo.Point{Lat: -35.10, Lon: -58.10}
	out := make(chan domain.Frame, 256)
	for i := range 90 {
		if i == 60 {
			p = geo.Destination(p, 45, 12*domain.NMToM) // 12 NM in one second
		}
		s.HandleLine(msg(3, "E0C0DE", "ARG1234", "24000", "", "", fmt.Sprintf("%.6f", p.Lat), fmt.Sprintf("%.6f", p.Lon), "", "4521", "0"),
			start.Add(time.Duration(i)*time.Second), out)
		s.HandleLine(msg(4, "E0C0DE", "", "", "447", "315", "", "", "0", "", ""), start.Add(time.Duration(i)*time.Second), out)
		p = geo.Destination(p, 315, 230)
	}
	close(out)
	var got []domain.AnomalyType
	for f := range out {
		for _, ev := range eng.Process(f.Strip()) {
			got = append(got, ev.Type)
		}
	}
	assert.Contains(t, got, domain.AnomalyKinematicImpossible)
}

func TestRunOverTCPWithReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for range 2 { // first connection drops after one line; the source reconnects
			c, err := ln.Accept()
			if err != nil {
				return
			}
			w := bufio.NewWriter(c)
			fmt.Fprintln(w, msg(3, "E3F00D", "", "5000", "", "", "-34.61", "-58.78", "", "", "0"))
			w.Flush()
			c.Close()
		}
	}()
	s, err := New(ln.Addr().String(), slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := make(chan domain.Frame, 4)
	go func() { _ = s.Run(ctx, out) }()
	for range 2 {
		select {
		case f := <-out:
			assert.Equal(t, "adsb:e3f00d", f.EntityKey)
		case <-ctx.Done():
			t.Fatal("no frame over TCP")
		}
	}
}

func TestNewValidatesAddress(t *testing.T) {
	_, err := New("30003", nil)
	assert.Error(t, err)
}
