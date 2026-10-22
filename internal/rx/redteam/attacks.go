// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package redteam

import (
	"errors"
	"math"
	"math/rand/v2"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// TimedFrame is a frame scheduled at an offset from the attack start.
type TimedFrame struct {
	At    time.Duration
	Frame domain.Frame
}

// Truth is one expected anomaly, written to groundtruth.jsonl.
type Truth struct {
	Scenario    string        `json:"scenario"`
	EntityKey   string        `json:"entity_key"`
	AnomalyType string        `json:"anomaly_type"`
	Start       time.Duration `json:"-"`
	End         time.Duration `json:"-"`
	TStart      time.Time     `json:"t_start"`
	TEnd        time.Time     `json:"t_end"`
}

// Track is the kinematic state of an aircraft or vessel used as a target.
type Track struct {
	Key          string
	Domain       domain.Domain
	Pos          geo.Point
	AltBaroFt    float64
	GeoOffsetFt  float64 // alt_geom − alt_baro under normal conditions
	SpeedMps     float64
	TrackDeg     float64
	Callsign     string
	Registration string
	AircraftType string
	Category     string
	Squawk       string
	NACp, NIC    int
}

// TrackFromFrame seeds a Track from a live frame (hijack mode).
func TrackFromFrame(f domain.Frame) Track {
	t := Track{
		Key: f.EntityKey, Domain: f.Domain, Pos: geo.Point{Lat: f.Lat, Lon: f.Lon},
		Callsign: f.Callsign, Registration: f.Registration, AircraftType: f.AircraftType,
		Category: f.Category, Squawk: f.Squawk, NACp: 9, NIC: 8,
	}
	if f.AltBaroFt != nil {
		t.AltBaroFt = *f.AltBaroFt
		if f.AltGeomFt != nil {
			t.GeoOffsetFt = *f.AltGeomFt - *f.AltBaroFt
		}
	}
	if f.GroundSpeedMps != nil {
		t.SpeedMps = *f.GroundSpeedMps
	}
	if f.TrackDeg != nil {
		t.TrackDeg = *f.TrackDeg
	}
	if f.NACp != nil {
		t.NACp = *f.NACp
	}
	if f.NIC != nil {
		t.NIC = *f.NIC
	}
	return t
}

// generator builds the attack timeline. target is nil when synthesizing.
type generator interface {
	validate() error
	synthTarget() Track
	build(name string, target Track, rng *rand.Rand) ([]TimedFrame, []Truth)
}

// ---------------------------------------------------------------------------
// shared helpers

// airFrame renders an aircraft state as a frame (timestamps filled by the runner).
func airFrame(t Track, altGeomFt float64) domain.Frame {
	// Real ADS-B position error is temporally correlated and smooth; white
	// per-frame noise would fabricate velocity spikes, so positions are exact
	// and only report timing jitters.
	f := domain.Frame{
		EntityKey: t.Key, Domain: domain.DomainAir, Lat: t.Pos.Lat, Lon: t.Pos.Lon,
		AltBaroFt: domain.Ptr(math.Round(t.AltBaroFt/25) * 25), AltGeomFt: domain.Ptr(math.Round(altGeomFt/25) * 25),
		GroundSpeedMps: domain.Ptr(t.SpeedMps), TrackDeg: domain.Ptr(geo.NormalizeDeg(t.TrackDeg)),
		OnGround: domain.Ptr(false), Callsign: t.Callsign, Registration: t.Registration,
		AircraftType: t.AircraftType, Category: t.Category, Squawk: t.Squawk,
	}
	if t.NACp > 0 {
		f.NACp = domain.Ptr(t.NACp)
	}
	if t.NIC > 0 {
		f.NIC = domain.Ptr(t.NIC)
	}
	return f
}

func step(t *Track, dt time.Duration) {
	t.Pos = geo.Destination(t.Pos, t.TrackDeg, t.SpeedMps*dt.Seconds())
}

func jitter(rng *rand.Rand) time.Duration {
	return time.Duration(rng.IntN(41)-20) * time.Millisecond
}

func syntheticAirliner(icao, callsign string) Track {
	// Westbound airliner on approach corridor south-east of SAEZ.
	return Track{
		Key: domain.ADSBKey(icao), Domain: domain.DomainAir, Pos: geo.Point{Lat: -35.10, Lon: -58.10},
		AltBaroFt: 24000, GeoOffsetFt: 375, SpeedMps: 230, TrackDeg: 315,
		Callsign: callsign, AircraftType: "A320", Category: "A3", Squawk: "4521", NACp: 9, NIC: 8,
	}
}

// ---------------------------------------------------------------------------
// 1. Kinematic impossibility

type kinematicParams struct {
	ICAO        string        `yaml:"icao24"`
	Callsign    string        `yaml:"callsign"`
	Mode        string        `yaml:"mode"` // "teleport" | "overspeed"
	Normal      time.Duration `yaml:"normal_duration"`
	JumpNM      float64       `yaml:"jump_nm"`
	JumpSeconds float64       `yaml:"jump_seconds"`
	PeakMps     float64       `yaml:"peak_mps"`
	RampSeconds float64       `yaml:"ramp_seconds"`
	Attack      time.Duration `yaml:"attack_duration"`
	RateHz      float64       `yaml:"rate_hz"`
}

func defaultKinematic() kinematicParams {
	return kinematicParams{ICAO: "e0c0de", Callsign: "ARG1234", Mode: "teleport", Normal: 60 * time.Second,
		JumpNM: 60, JumpSeconds: 2, PeakMps: 1400, RampSeconds: 10, Attack: 60 * time.Second, RateHz: 1}
}

func (p *kinematicParams) validate() error {
	if p.Mode != "teleport" && p.Mode != "overspeed" {
		return errors.New("kinematic.mode must be teleport or overspeed")
	}
	if p.RateHz <= 0 || p.JumpSeconds <= 0 || p.RampSeconds <= 0 {
		return errors.New("kinematic: rate_hz, jump_seconds, ramp_seconds must be > 0")
	}
	return nil
}

func (p *kinematicParams) synthTarget() Track { return syntheticAirliner(p.ICAO, p.Callsign) }

// When hijacking, the live track already provides the normal phase.
func (p *kinematicParams) hijacked() { p.Normal = 0 }

func (p *kinematicParams) build(name string, t Track, rng *rand.Rand) ([]TimedFrame, []Truth) {
	dt := time.Duration(float64(time.Second) / p.RateHz)
	var out []TimedFrame
	at := time.Duration(0)
	emit := func() {
		out = append(out, TimedFrame{At: at + jitter(rng), Frame: airFrame(t, t.AltBaroFt+t.GeoOffsetFt)})
	}
	for ; at < p.Normal; at += dt {
		emit()
		step(&t, dt)
	}
	attackStart := at
	switch p.Mode {
	case "teleport":
		at += time.Duration(p.JumpSeconds * float64(time.Second))
		t.Pos = geo.Destination(t.Pos, t.TrackDeg+90, p.JumpNM*domain.NMToM)
		for end := attackStart + p.Attack; at < end; at += dt {
			emit()
			step(&t, dt)
		}
	case "overspeed":
		base := t.SpeedMps
		for end := attackStart + p.Attack; at < end; at += dt {
			frac := math.Min(1, (at-attackStart).Seconds()/p.RampSeconds)
			t.SpeedMps = base + frac*(p.PeakMps-base)
			emit()
			step(&t, dt)
		}
	}
	return out, []Truth{{Scenario: name, EntityKey: t.Key, AnomalyType: "KINEMATIC_IMPOSSIBLE", Start: attackStart, End: at}}
}

// ---------------------------------------------------------------------------
// 2. Baro/GNSS divergence (GPS spoofing signature)

type baroGNSSParams struct {
	ICAO         string        `yaml:"icao24"`
	Callsign     string        `yaml:"callsign"`
	Normal       time.Duration `yaml:"normal_duration"`
	RampFtPerSec float64       `yaml:"ramp_ft_per_s"`
	MaxOffsetFt  float64       `yaml:"max_offset_ft"`
	NACpSpoofed  int           `yaml:"nac_p_spoofed"`
	// Optional synthetic-airliner placement (zero values keep the default track).
	StartLat   float64       `yaml:"start_lat"`
	StartLon   float64       `yaml:"start_lon"`
	HeadingDeg float64       `yaml:"heading_deg"`
	NICSpoofed int           `yaml:"nic_spoofed"`
	Attack     time.Duration `yaml:"attack_duration"`
	RateHz     float64       `yaml:"rate_hz"`
}

func defaultBaroGNSS() baroGNSSParams {
	return baroGNSSParams{ICAO: "e0babe", Callsign: "ARG5678", Normal: 60 * time.Second, RampFtPerSec: 80,
		MaxOffsetFt: 3000, NACpSpoofed: 4, NICSpoofed: 3, Attack: 90 * time.Second, RateHz: 1}
}

func (p *baroGNSSParams) validate() error {
	if p.RateHz <= 0 || p.RampFtPerSec <= 0 || p.MaxOffsetFt <= 0 {
		return errors.New("baro_gnss: rate_hz, ramp_ft_per_s, max_offset_ft must be > 0")
	}
	return nil
}

func (p *baroGNSSParams) synthTarget() Track {
	t := syntheticAirliner(p.ICAO, p.Callsign)
	if p.StartLat != 0 || p.StartLon != 0 {
		t.Pos = geo.Point{Lat: p.StartLat, Lon: p.StartLon}
	}
	if p.HeadingDeg != 0 {
		t.TrackDeg = p.HeadingDeg
	}
	return t
}

func (p *baroGNSSParams) hijacked() { p.Normal = 0 }

func (p *baroGNSSParams) build(name string, t Track, rng *rand.Rand) ([]TimedFrame, []Truth) {
	dt := time.Duration(float64(time.Second) / p.RateHz)
	var out []TimedFrame
	at := time.Duration(0)
	for ; at < p.Normal; at += dt {
		out = append(out, TimedFrame{At: at + jitter(rng), Frame: airFrame(t, t.AltBaroFt+t.GeoOffsetFt)})
		step(&t, dt)
	}
	attackStart := at
	t.NACp, t.NIC = p.NACpSpoofed, p.NICSpoofed
	for end := attackStart + p.Attack; at < end; at += dt {
		offset := math.Min(p.MaxOffsetFt, p.RampFtPerSec*(at-attackStart).Seconds())
		out = append(out, TimedFrame{At: at + jitter(rng), Frame: airFrame(t, t.AltBaroFt+t.GeoOffsetFt+offset)})
		step(&t, dt)
	}
	return out, []Truth{
		{Scenario: name, EntityKey: t.Key, AnomalyType: "BARO_GNSS_DIVERGENCE", Start: attackStart, End: at},
		{Scenario: name, EntityKey: t.Key, AnomalyType: "NAV_INTEGRITY_DROP", Start: attackStart, End: at},
	}
}

// ---------------------------------------------------------------------------
// 3. ADS-B ghost near SADM

type ghostParams struct {
	ICAO     string `yaml:"icao24"`
	Callsign string `yaml:"callsign"` // empty = ghost booster
	Spawn    struct {
		Ref        string  `yaml:"ref"`
		BearingDeg float64 `yaml:"bearing_deg"`
		RangeNM    float64 `yaml:"range_nm"`
	} `yaml:"spawn"`
	AltFt      float64       `yaml:"alt_ft"`
	SpeedKt    float64       `yaml:"speed_kt"`
	HeadingDeg float64       `yaml:"heading_deg"`
	LegNM      float64       `yaml:"leg_nm"`
	Duration   time.Duration `yaml:"duration"`
	RateHz     float64       `yaml:"rate_hz"`
	NACp       int           `yaml:"nac_p"` // 0 = not reported
}

// aerodromeRefs holds the reference points scenarios may spawn relative to.
var aerodromeRefs = map[string]geo.Point{
	"SADM": {Lat: -34.6763, Lon: -58.6428},
	"SAEZ": {Lat: -34.8222, Lon: -58.5358},
	"SABE": {Lat: -34.5592, Lon: -58.4156},
}

func defaultGhost() ghostParams {
	p := ghostParams{ICAO: "e3f00d", AltFt: 5000, SpeedKt: 210, HeadingDeg: 120, LegNM: 6,
		Duration: 180 * time.Second, RateHz: 1}
	p.Spawn.Ref, p.Spawn.BearingDeg, p.Spawn.RangeNM = "SADM", 300, 8
	return p
}

func (p *ghostParams) validate() error {
	if _, ok := aerodromeRefs[p.Spawn.Ref]; !ok {
		return errors.New("ghost.spawn.ref must be SADM, SAEZ or SABE")
	}
	if p.RateHz <= 0 || p.SpeedKt <= 0 || p.LegNM <= 0 {
		return errors.New("ghost: rate_hz, speed_kt, leg_nm must be > 0")
	}
	return nil
}

func (p *ghostParams) synthTarget() Track {
	return Track{
		Key: domain.ADSBKey(p.ICAO), Domain: domain.DomainAir,
		Pos:       geo.Destination(aerodromeRefs[p.Spawn.Ref], p.Spawn.BearingDeg, p.Spawn.RangeNM*domain.NMToM),
		AltBaroFt: p.AltFt, GeoOffsetFt: 150, SpeedMps: p.SpeedKt * domain.KnotToMps, TrackDeg: p.HeadingDeg,
		Callsign: p.Callsign, Category: "A2", NACp: p.NACp,
	}
}

func (p *ghostParams) build(name string, t Track, rng *rand.Rand) ([]TimedFrame, []Truth) {
	dt := time.Duration(float64(time.Second) / p.RateHz)
	legDur := time.Duration(p.LegNM * domain.NMToM / t.SpeedMps * float64(time.Second))
	const turnRate = 3.0 // deg/s, standard rate
	turnDur := time.Duration(180 / turnRate * float64(time.Second))
	var out []TimedFrame
	for at := time.Duration(0); at < p.Duration; at += dt {
		out = append(out, TimedFrame{At: at + jitter(rng), Frame: airFrame(t, t.AltBaroFt+t.GeoOffsetFt)})
		// Racetrack: straight leg, then a 180° standard-rate turn.
		if phase := at % (legDur + turnDur); phase >= legDur {
			t.TrackDeg = geo.NormalizeDeg(t.TrackDeg + turnRate*dt.Seconds())
		}
		step(&t, dt)
	}
	return out, []Truth{{Scenario: name, EntityKey: t.Key, AnomalyType: "GHOST_TRACK", Start: 0, End: p.Duration}}
}

// ---------------------------------------------------------------------------
// 4. EEZ dark vessel

type darkVesselParams struct {
	MMSI         string        `yaml:"mmsi"`
	ShipName     string        `yaml:"ship_name"`
	ShipType     int           `yaml:"ship_type"`
	LengthM      float64       `yaml:"length_m"`
	StartLat     float64       `yaml:"start_lat"`
	StartLon     float64       `yaml:"start_lon"`
	CourseDeg    float64       `yaml:"course_deg"`
	SpeedKt      float64       `yaml:"speed_kt"`
	Cruise       time.Duration `yaml:"cruise_duration"`
	TurnDeg      float64       `yaml:"turn_deg"` // signed; negative = to port
	ReportsAfter int           `yaml:"reports_after_turn"`
	Interval     time.Duration `yaml:"report_interval"`
	DarkMinGap   time.Duration `yaml:"dark_min_gap"` // used for ground truth only
	Horizon      time.Duration `yaml:"truth_horizon"`
}

func defaultDarkVessel() darkVesselParams {
	return darkVesselParams{MMSI: "200000001", ShipName: "SYNTHETIC SQUID 1", ShipType: 30, LengthM: 65,
		StartLat: -46.40, StartLon: -60.80, CourseDeg: 15, SpeedKt: 9, Cruise: 10 * time.Minute,
		TurnDeg: -70, ReportsAfter: 2, Interval: 10 * time.Second, DarkMinGap: 10 * time.Minute, Horizon: 60 * time.Minute}
}

func (p *darkVesselParams) validate() error {
	if p.MMSI == "" || p.Interval <= 0 || p.SpeedKt <= 0 {
		return errors.New("dark_vessel: mmsi, report_interval, speed_kt required")
	}
	return nil
}

func (p *darkVesselParams) synthTarget() Track {
	return Track{Key: domain.AISKey(p.MMSI), Domain: domain.DomainSea, Pos: geo.Point{Lat: p.StartLat, Lon: p.StartLon},
		SpeedMps: p.SpeedKt * domain.KnotToMps, TrackDeg: p.CourseDeg}
}

func (p *darkVesselParams) build(name string, t Track, rng *rand.Rand) ([]TimedFrame, []Truth) {
	frame := func() domain.Frame {
		return domain.Frame{
			EntityKey: t.Key, Domain: domain.DomainSea, Lat: t.Pos.Lat, Lon: t.Pos.Lon,
			GroundSpeedMps: domain.Ptr(t.SpeedMps), TrackDeg: domain.Ptr(geo.NormalizeDeg(t.TrackDeg)),
			HeadingDeg: domain.Ptr(math.Round(geo.NormalizeDeg(t.TrackDeg))),
			MMSI:       p.MMSI, ShipName: p.ShipName, ShipType: domain.Ptr(p.ShipType), AISClass: "A",
			NavStatus: domain.Ptr(7), // engaged in fishing
			LengthM:   domain.Ptr(p.LengthM),
		}
	}
	var out []TimedFrame
	at := time.Duration(0)
	for ; at < p.Cruise; at += p.Interval {
		out = append(out, TimedFrame{At: at + jitter(rng), Frame: frame()})
		step(&t, p.Interval)
	}
	t.TrackDeg = geo.NormalizeDeg(t.TrackDeg + p.TurnDeg)
	for range p.ReportsAfter {
		out = append(out, TimedFrame{At: at + jitter(rng), Frame: frame()})
		step(&t, p.Interval)
		at += p.Interval
	}
	lastReport := at - p.Interval
	return out, []Truth{{Scenario: name, EntityKey: t.Key, AnomalyType: "AIS_DARK_EEZ",
		Start: lastReport + p.DarkMinGap, End: lastReport + p.Horizon}}
}
