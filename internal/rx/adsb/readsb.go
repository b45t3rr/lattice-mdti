// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package adsb ingests ADS-B traffic from readsb/ADSBx-v2-compatible HTTP
// aggregators and turns periodic snapshots into a stream of new position
// reports (SPEC §4.1).
package adsb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// snapshot is the readsb JSON envelope. adsb.fi v3 and adsb.lol v2 use "ac";
// older endpoints use "aircraft".
type snapshot struct {
	Now      float64    `json:"now"` // epoch milliseconds
	AC       []aircraft `json:"ac"`
	Aircraft []aircraft `json:"aircraft"`
}

type aircraft struct {
	Hex         string          `json:"hex"`
	Type        string          `json:"type"`
	Flight      string          `json:"flight"`
	R           string          `json:"r"`
	T           string          `json:"t"`
	AltBaro     json.RawMessage `json:"alt_baro"` // number or "ground"
	AltGeom     *float64        `json:"alt_geom"`
	GS          *float64        `json:"gs"`
	Track       *float64        `json:"track"`
	TrueHeading *float64        `json:"true_heading"`
	BaroRate    *float64        `json:"baro_rate"`
	GeomRate    *float64        `json:"geom_rate"`
	Squawk      string          `json:"squawk"`
	Category    string          `json:"category"`
	Lat         *float64        `json:"lat"`
	Lon         *float64        `json:"lon"`
	NIC         *int            `json:"nic"`
	NACp        *int            `json:"nac_p"`
	SIL         *int            `json:"sil"`
	SeenPos     *float64        `json:"seen_pos"` // seconds since last position
	MLAT        []string        `json:"mlat"`     // fields derived from MLAT
	TISB        []string        `json:"tisb"`     // fields derived from TIS-B
}

// Parse decodes a readsb snapshot into frames. Aircraft without a position are
// skipped. receivedAt is the local receive time.
func Parse(body []byte, source domain.Source, receivedAt time.Time) ([]domain.Frame, error) {
	var s snapshot
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("decode readsb snapshot: %w", err)
	}
	list := s.AC
	if len(list) == 0 {
		list = s.Aircraft
	}
	ref := receivedAt
	if s.Now > 0 {
		ref = time.UnixMilli(int64(s.Now))
	}
	frames := make([]domain.Frame, 0, len(list))
	for _, a := range list {
		if f, ok := a.frame(source, ref, receivedAt); ok {
			frames = append(frames, f)
		}
	}
	return frames, nil
}

func (a aircraft) frame(source domain.Source, ref, receivedAt time.Time) (domain.Frame, bool) {
	hex := strings.ToLower(strings.TrimSpace(a.Hex))
	if hex == "" || a.Lat == nil || a.Lon == nil || a.SeenPos == nil {
		return domain.Frame{}, false
	}
	f := domain.Frame{
		EntityKey:       domain.ADSBKey(hex),
		Domain:          domain.DomainAir,
		Source:          source,
		ObservedAt:      ref.Add(-time.Duration(*a.SeenPos * float64(time.Second))),
		ReceivedAt:      receivedAt,
		Lat:             *a.Lat,
		Lon:             *a.Lon,
		TrackDeg:        a.Track,
		HeadingDeg:      a.TrueHeading,
		AltGeomFt:       a.AltGeom,
		VertRateBaroFpm: a.BaroRate,
		VertRateGeomFpm: a.GeomRate,
		Callsign:        domain.SanitizeIdent(a.Flight),
		Registration:    domain.SanitizeIdent(a.R),
		AircraftType:    domain.SanitizeIdent(a.T),
		Squawk:          sanitizeSquawk(a.Squawk),
		Category:        domain.SanitizeIdent(a.Category),
		NIC:             a.NIC,
		NACp:            a.NACp,
		SIL:             a.SIL,
		IsMLAT:          a.Type == "mlat" || slices.Contains(a.MLAT, "lat"),
		IsTISB:          strings.HasPrefix(a.Type, "tisb") || strings.HasPrefix(hex, "~") || slices.Contains(a.TISB, "lat"),
	}
	if a.GS != nil {
		f.GroundSpeedMps = domain.Ptr(*a.GS * domain.KnotToMps)
	}
	switch raw := bytes.TrimSpace(a.AltBaro); {
	case len(raw) == 0 || string(raw) == "null":
	case string(raw) == `"ground"`:
		f.OnGround = domain.Ptr(true)
	default:
		var alt float64
		if err := json.Unmarshal(raw, &alt); err == nil && !math.IsNaN(alt) {
			f.AltBaroFt = &alt
			f.OnGround = domain.Ptr(false)
		}
	}
	return f, true
}

// sanitizeSquawk keeps only a 4-digit octal code.
func sanitizeSquawk(s string) string {
	s = strings.TrimSpace(s)
	if len(s) != 4 {
		return ""
	}
	for _, r := range s {
		if r < '0' || r > '7' {
			return ""
		}
	}
	return s
}
