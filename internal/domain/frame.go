// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package domain holds MDTI's transport-agnostic contracts (SPEC §3).
// It must not import Lattice or OpenAI types.
package domain

import (
	"fmt"
	"strings"
	"time"
)

// Domain is the physical environment a track lives in.
type Domain string

const (
	DomainAir Domain = "AIR"
	DomainSea Domain = "SEA"
)

// Source identifies where a frame came from. It is visible to RX and the
// ground-truth writer only; Strip() clears it before Tier 1/Tier 2 (SPEC §4.4).
type Source string

const (
	SourceADSBFi    Source = "adsbfi"
	SourceADSBLol   Source = "adsblol"
	SourceAISStream Source = "aisstream"
	SourceRedTeam   Source = "redteam"
	SourceReplay    Source = "replay"
	SourceSynthetic Source = "synthetic" // offline demo background traffic
	SourceNMEA      Source = "nmea"      // local AIS receiver (NMEA 0183 !AIVDM)
	SourceSBS       Source = "sbs"       // local ADS-B receiver (BaseStation port 30003)
	SourceReadsb    Source = "readsb"    // local ADS-B receiver (readsb/dump1090 aircraft.json)
)

// Frame is one normalized telemetry sample (SPEC §3.1).
// Pointer fields mean "not reported by the source" and are never zero-filled.
type Frame struct {
	EntityKey  string    `json:"entity_key"`
	Domain     Domain    `json:"domain"`
	Source     Source    `json:"source,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	ReceivedAt time.Time `json:"received_at"`

	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`

	GroundSpeedMps *float64 `json:"gs_mps,omitempty"`
	TrackDeg       *float64 `json:"track_deg,omitempty"`
	HeadingDeg     *float64 `json:"heading_deg,omitempty"`

	// AIR
	AltBaroFt       *float64 `json:"alt_baro_ft,omitempty"`
	AltGeomFt       *float64 `json:"alt_geom_ft,omitempty"`
	VertRateBaroFpm *float64 `json:"baro_rate_fpm,omitempty"`
	VertRateGeomFpm *float64 `json:"geom_rate_fpm,omitempty"`
	OnGround        *bool    `json:"on_ground,omitempty"`
	Callsign        string   `json:"callsign,omitempty"`     // UNTRUSTED
	Squawk          string   `json:"squawk,omitempty"`       // 4 octal digits as text
	Category        string   `json:"category,omitempty"`     // ADS-B emitter category, e.g. "A3"
	Registration    string   `json:"registration,omitempty"` // UNTRUSTED
	AircraftType    string   `json:"aircraft_type,omitempty"`
	NIC             *int     `json:"nic,omitempty"`
	NACp            *int     `json:"nac_p,omitempty"`
	SIL             *int     `json:"sil,omitempty"`
	IsMLAT          bool     `json:"is_mlat,omitempty"`
	IsTISB          bool     `json:"is_tisb,omitempty"`

	// SEA
	MMSI      string   `json:"mmsi,omitempty"`
	IMO       string   `json:"imo,omitempty"`
	ShipName  string   `json:"ship_name,omitempty"` // UNTRUSTED
	ShipCall  string   `json:"ship_callsign,omitempty"`
	ShipType  *int     `json:"ship_type,omitempty"`
	NavStatus *int     `json:"nav_status,omitempty"`
	AISClass  string   `json:"ais_class,omitempty"` // "A" | "B"
	LengthM   *float64 `json:"length_m,omitempty"`
	BeamM     *float64 `json:"beam_m,omitempty"`
}

// ADSBKey builds the entity key for an ICAO 24-bit address (lowercase hex).
func ADSBKey(icao24 string) string { return "adsb:" + icao24 }

// AISKey builds the entity key for an MMSI.
func AISKey(mmsi string) string { return "ais:" + mmsi }

// Strip returns a copy with provenance-revealing fields cleared, for consumers
// that must not learn whether a frame was injected.
func (f Frame) Strip() Frame {
	f.Source = ""
	return f
}

// Boundary validation limits (SPEC §3.1).
const (
	MaxFutureSkew = 5 * time.Second
	MaxFrameAge   = 120 * time.Second
)

// Validate rejects frames that must never enter the pipeline.
func (f Frame) Validate(now time.Time) error {
	switch {
	case f.EntityKey == "":
		return fmt.Errorf("empty entity key")
	case f.Domain != DomainAir && f.Domain != DomainSea:
		return fmt.Errorf("invalid domain %q", f.Domain)
	case f.Lat < -90 || f.Lat > 90 || f.Lon < -180 || f.Lon > 180:
		return fmt.Errorf("position out of range (%f,%f)", f.Lat, f.Lon)
	case f.Lat == 0 && f.Lon == 0:
		return fmt.Errorf("null island position")
	case f.ObservedAt.IsZero():
		return fmt.Errorf("missing observation time")
	case f.ObservedAt.After(now.Add(MaxFutureSkew)):
		return fmt.Errorf("observation %s in the future", f.ObservedAt.Sub(now))
	case now.Sub(f.ObservedAt) > MaxFrameAge:
		return fmt.Errorf("observation %s old", now.Sub(f.ObservedAt).Round(time.Second))
	}
	return nil
}

// Unit conversions used across RX.
const (
	KnotToMps = 0.514444
	FtToM     = 0.3048
	NMToM     = 1852.0
)

// Ptr returns a pointer to v (convenience for optional fields).
func Ptr[T any](v T) *T { return &v }

// EntityID maps an entity key to its deterministic Lattice entity id
// ("adsb:e1724f" → "mdti-adsb-e1724f"). It is the upsert key (SPEC §5.3).
func EntityID(key string) string { return "mdti-" + strings.Replace(key, ":", "-", 1) }
