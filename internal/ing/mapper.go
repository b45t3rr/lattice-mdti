// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package ing is the Lattice entity sync engine (SPEC §5): it maps frames to
// Lattice entities and keeps them alive with correct expiry.
package ing

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

// Expiry policy (SPEC §5.3).
const (
	ADSBExpiry = 60 * time.Second
	AISExpiry  = 15 * time.Minute
	DRExpiry   = 5 * time.Minute
	// MaxClockSkew: beyond this, expiry is computed from the local clock.
	MaxClockSkew = 5 * time.Second
)

// Mapper converts frames into Lattice entities.
type Mapper struct {
	IntegrationName string
	Now             func() time.Time
}

func (m *Mapper) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Entity maps a frame to a complete TEMPLATE_TRACK entity. createdAt is when
// MDTI first saw the track.
func (m *Mapper) Entity(f domain.Frame, createdAt time.Time) *Lattice.Entity {
	now := m.now()
	base := f.ObservedAt
	if d := now.Sub(base); d > MaxClockSkew || d < -MaxClockSkew {
		base = now
	}
	env := Lattice.MilViewEnvironmentEnvironmentAir
	expiry := ADSBExpiry
	dataType := "adsb"
	if f.Domain == domain.DomainSea {
		env = Lattice.MilViewEnvironmentEnvironmentSurface
		expiry = AISExpiry
		dataType = "ais"
	}
	disp := InitialDisposition(f)
	platform, specific, fn := classify(f)

	e := &Lattice.Entity{
		EntityID:    Lattice.String(domain.EntityID(f.EntityKey)),
		Description: Lattice.String(description(f, platform)),
		IsLive:      Lattice.Bool(true),
		CreatedTime: &createdAt,
		ExpiryTime:  ptr(base.Add(expiry)),
		Aliases:     aliases(f),
		Location:    location(f),
		Ontology: &Lattice.Ontology{
			Template:     ptr(Lattice.OntologyTemplateTemplateTrack),
			PlatformType: Lattice.String(platform),
		},
		MilView: &Lattice.MilView{Disposition: &disp, Environment: &env},
		Tracked: &Lattice.Tracked{
			TrackQualityWrapper: Lattice.Int(trackQuality(f)),
			LastMeasurementTime: ptr(f.ObservedAt),
		},
		Symbology: &Lattice.Symbology{MilStd2525C: &Lattice.MilStd2525C{
			Sidc: Lattice.String(lattice.SIDC(disp, env, fn)),
		}},
		DataClassification: &Lattice.Classification{Default: &Lattice.ClassificationInformation{
			Level: ptr(Lattice.ClassificationInformationLevelClassificationLevelsUnclassified),
		}},
		Provenance: &Lattice.Provenance{
			IntegrationName: Lattice.String(m.IntegrationName),
			DataType:        Lattice.String(dataType),
			// The server overwrites sourceId with the authenticated principal, so
			// the frame origin (adsbfi, aisstream, redteam, …) goes in the description.
			SourceDescription: Lattice.String(string(f.Source)),
			SourceUpdateTime:  ptr(f.ObservedAt),
		},
	}
	if specific != "" {
		e.Ontology.SpecificType = Lattice.String(specific)
	}
	// Injected and synthetic tracks are labelled as simulated in the C2
	// ("S" badge, "Simulated" in the tooltip). Tier 1/2 never see the origin.
	if f.Source == domain.SourceRedTeam || f.Source == domain.SourceSynthetic {
		e.Indicators = &Lattice.Indicators{Simulated: Lattice.Bool(true)}
	}
	if f.Domain == domain.DomainAir {
		e.TransponderCodes = transponder(f)
	} else {
		if n, ok := lattice.NationalityFromMMSI(f.MMSI); ok {
			e.MilView.Nationality = &n
		}
		if f.LengthM != nil {
			e.Dimensions = &Lattice.Dimensions{LengthM: f.LengthM}
		}
	}
	return e
}

// InitialDisposition implements SPEC §3.4.1. ING never publishes SUSPICIOUS or
// HOSTILE: those exist only as C2 overrides.
func InitialDisposition(f domain.Frame) Lattice.MilViewDisposition {
	switch f.Domain {
	case domain.DomainAir:
		cooperative := f.Callsign != "" && f.Squawk != "" && !f.IsMLAT && !f.IsTISB
		if cooperative && len(f.Category) == 2 && f.Category[0] == 'A' && f.Category[1] >= '1' && f.Category[1] <= '5' {
			return Lattice.MilViewDispositionDispositionNeutral
		}
	case domain.DomainSea:
		if f.ShipName != "" {
			return Lattice.MilViewDispositionDispositionNeutral
		}
	}
	return Lattice.MilViewDispositionDispositionUnknown
}

var categoryPlatform = map[string]string{
	"A1": "Light aircraft", "A2": "Small aircraft", "A3": "Airliner", "A4": "Heavy airliner",
	"A5": "Heavy airliner", "A6": "High performance aircraft", "A7": "Rotorcraft", "B6": "UAV",
}

func classify(f domain.Frame) (platform, specific string, fn lattice.Function) {
	if f.Domain == domain.DomainAir {
		platform = categoryPlatform[f.Category]
		if platform == "" {
			platform = "Aircraft"
		}
		switch f.Category {
		case "A7":
			fn = lattice.FuncCivilRotary
		case "B6":
			fn = lattice.FuncUnspecified // a UAV's allegiance is unknown: no civil/military function code
		default:
			fn = lattice.FuncCivilFixedWing
		}
		return platform, f.AircraftType, fn
	}
	fn = lattice.FuncNonMilitarySea
	platform = "Vessel"
	if f.ShipType != nil {
		switch t := *f.ShipType; {
		case t == 30:
			platform, fn = "Fishing Vessel", lattice.FuncFishingVessel
		case t >= 60 && t <= 69:
			platform = "Passenger Vessel"
		case t >= 70 && t <= 79:
			platform, fn = "Cargo Vessel", lattice.FuncNonMilitaryCargo
		case t >= 80 && t <= 89:
			platform = "Tanker"
		case t == 31 || t == 32 || t == 52:
			platform = "Tug"
		case t == 35:
			platform, fn = "Military Vessel", lattice.FuncUnspecified
		case t == 36:
			platform = "Sailing Vessel"
		case t == 37:
			platform = "Pleasure Craft"
		}
	}
	return platform, "", fn
}

func description(f domain.Frame, platform string) string {
	var parts []string
	if f.Domain == domain.DomainAir {
		parts = append(parts, nonEmpty(f.AircraftType, platform), f.Callsign)
	} else {
		parts = append(parts, platform, f.ShipName)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func aliases(f domain.Frame) *Lattice.Aliases {
	a := &Lattice.Aliases{}
	add := func(id string, t Lattice.AlternateIDType) {
		if id != "" {
			a.AlternateIDs = append(a.AlternateIDs, &Lattice.AlternateID{ID: Lattice.String(id), Type: &t})
		}
	}
	if f.Domain == domain.DomainAir {
		icao := strings.TrimPrefix(f.EntityKey, "adsb:")
		a.Name = Lattice.String(nonEmpty(f.Callsign, "ICAO "+strings.ToUpper(icao)))
		add(f.Callsign, Lattice.AlternateIDTypeAltIDTypeCallsign)
		add(f.Registration, Lattice.AlternateIDTypeAltIDTypeRegistrationID)
	} else {
		a.Name = Lattice.String(nonEmpty(f.ShipName, "MMSI "+f.MMSI))
		add(f.MMSI, Lattice.AlternateIDTypeAltIDTypeMmsiID)
		add(f.IMO, Lattice.AlternateIDTypeAltIDTypeImoID)
		add(f.ShipCall, Lattice.AlternateIDTypeAltIDTypeCallsign)
	}
	return a
}

func location(f domain.Frame) *Lattice.Location {
	pos := &Lattice.Position{LatitudeDegrees: Lattice.Float64(f.Lat), LongitudeDegrees: Lattice.Float64(f.Lon)}
	switch {
	case f.AltGeomFt != nil:
		pos.AltitudeHaeMeters = Lattice.Float64(*f.AltGeomFt * domain.FtToM)
	case f.AltBaroFt != nil:
		pos.AltitudeHaeMeters = Lattice.Float64(*f.AltBaroFt * domain.FtToM)
	}
	loc := &Lattice.Location{Position: pos}
	if f.GroundSpeedMps != nil {
		loc.SpeedMps = f.GroundSpeedMps
		if f.TrackDeg != nil {
			rad := *f.TrackDeg * math.Pi / 180
			vel := &Lattice.Enu{
				E: Lattice.Float64(*f.GroundSpeedMps * math.Sin(rad)),
				N: Lattice.Float64(*f.GroundSpeedMps * math.Cos(rad)),
			}
			switch {
			case f.VertRateGeomFpm != nil:
				vel.U = Lattice.Float64(*f.VertRateGeomFpm * domain.FtToM / 60)
			case f.VertRateBaroFpm != nil:
				vel.U = Lattice.Float64(*f.VertRateBaroFpm * domain.FtToM / 60)
			}
			loc.VelocityEnu = vel
		}
	}
	heading := f.HeadingDeg
	if heading == nil {
		heading = f.TrackDeg
	}
	if heading != nil {
		loc.AttitudeEnu = yawQuaternion(*heading)
	}
	return loc
}

// yawQuaternion converts a true heading (0 = north, clockwise) to an ENU
// attitude quaternion. ENU yaw is counter-clockwise from east: yaw = 90° − hdg.
func yawQuaternion(headingDeg float64) *Lattice.Quaternion {
	yaw := (90 - headingDeg) * math.Pi / 180
	return &Lattice.Quaternion{
		X: Lattice.Float64(0), Y: Lattice.Float64(0),
		Z: Lattice.Float64(math.Sin(yaw / 2)), W: Lattice.Float64(math.Cos(yaw / 2)),
	}
}

func transponder(f domain.Frame) *Lattice.TransponderCodes {
	tc := &Lattice.TransponderCodes{}
	set := false
	icao := strings.TrimPrefix(f.EntityKey, "adsb:")
	if !f.IsTISB && !strings.HasPrefix(icao, "~") {
		if addr, err := strconv.ParseInt(icao, 16, 32); err == nil && addr >= 1 && addr <= 16777214 {
			tc.ModeS = &Lattice.ModeS{Address: Lattice.Int(int(addr))}
			set = true
		}
	}
	if f.Squawk != "" {
		// Encoded as the four octal digits read as a decimal number (7500 → 7500);
		// see SPEC §12 Q5.
		if n, err := strconv.Atoi(f.Squawk); err == nil {
			tc.Mode3 = Lattice.Int(n)
			set = true
		}
	}
	if f.AltBaroFt != nil {
		tc.ModeCAltitudeFt = Lattice.Int(int(math.Round(*f.AltBaroFt)))
		set = true
	}
	if !set {
		return nil
	}
	return tc
}

// trackQuality maps navigation accuracy to Lattice track quality (SPEC §3.4.2).
func trackQuality(f domain.Frame) int {
	if f.Domain == domain.DomainSea {
		if f.AISClass == "A" {
			return 3
		}
		return 2
	}
	switch {
	case f.IsMLAT || f.IsTISB || f.NACp == nil:
		return 2
	case *f.NACp >= 8:
		return 4
	case *f.NACp >= 5:
		return 3
	default:
		return 2
	}
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

func ptr[T any](v T) *T { return &v }

// String renders an entity id + update time for logs.
func label(e *Lattice.Entity) string {
	t := ""
	if e.Provenance != nil && e.Provenance.SourceUpdateTime != nil {
		t = e.Provenance.SourceUpdateTime.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s@%s", *e.EntityID, t)
}
