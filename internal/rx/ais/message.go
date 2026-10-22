// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package ais ingests AIS traffic from the AISStream WebSocket (SPEC §4.2).
package ais

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Message types MDTI subscribes to.
const (
	TypePositionReport         = "PositionReport"
	TypeStandardClassBPosition = "StandardClassBPositionReport"
	TypeExtendedClassBPosition = "ExtendedClassBPositionReport"
	TypeShipStaticData         = "ShipStaticData"
	TypeStaticDataReport       = "StaticDataReport"
)

// SubscribedTypes is sent in the subscription filter.
var SubscribedTypes = []string{
	TypePositionReport, TypeStandardClassBPosition, TypeExtendedClassBPosition,
	TypeShipStaticData, TypeStaticDataReport,
}

// envelope mirrors aisstream/ais-message-models. MetaData is untyped upstream,
// so it is decoded leniently.
type envelope struct {
	MessageType string                     `json:"MessageType"`
	MetaData    map[string]json.RawMessage `json:"MetaData"`
	Message     map[string]json.RawMessage `json:"Message"`
}

type dimension struct{ A, B, C, D int }

type position struct {
	UserID             int64   `json:"UserID"`
	Valid              bool    `json:"Valid"`
	NavigationalStatus *int    `json:"NavigationalStatus"`
	Sog                float64 `json:"Sog"`
	Cog                float64 `json:"Cog"`
	TrueHeading        int     `json:"TrueHeading"`
	Latitude           float64 `json:"Latitude"`
	Longitude          float64 `json:"Longitude"`
	// ExtendedClassBPositionReport also carries static fields.
	Name      string     `json:"Name"`
	Type      *int       `json:"Type"`
	Dimension *dimension `json:"Dimension"`
}

type shipStatic struct {
	UserID    int64      `json:"UserID"`
	Valid     bool       `json:"Valid"`
	ImoNumber int64      `json:"ImoNumber"`
	CallSign  string     `json:"CallSign"`
	Name      string     `json:"Name"`
	Type      *int       `json:"Type"`
	Dimension *dimension `json:"Dimension"`
}

type staticReport struct {
	UserID  int64 `json:"UserID"`
	Valid   bool  `json:"Valid"`
	ReportA struct {
		Valid bool   `json:"Valid"`
		Name  string `json:"Name"`
	} `json:"ReportA"`
	ReportB struct {
		Valid     bool       `json:"Valid"`
		ShipType  int        `json:"ShipType"`
		CallSign  string     `json:"CallSign"`
		Dimension *dimension `json:"Dimension"`
	} `json:"ReportB"`
}

// Static is the per-MMSI static data cached and joined onto positions.
type Static struct {
	Name     string
	CallSign string
	IMO      string
	ShipType *int
	LengthM  *float64
	BeamM    *float64
}

func (s *Static) merge(o Static) {
	if o.Name != "" {
		s.Name = o.Name
	}
	if o.CallSign != "" {
		s.CallSign = o.CallSign
	}
	if o.IMO != "" {
		s.IMO = o.IMO
	}
	if o.ShipType != nil {
		s.ShipType = o.ShipType
	}
	if o.LengthM != nil {
		s.LengthM = o.LengthM
	}
	if o.BeamM != nil {
		s.BeamM = o.BeamM
	}
}

// Decoded is the result of decoding one AISStream message: a position frame
// (without static data joined yet), a static update, or both.
type Decoded struct {
	MMSI   string
	Frame  *domain.Frame
	Static *Static
}

// AIS "not available" sentinels (ITU-R M.1371).
const (
	sogUnavailable     = 102.3
	cogUnavailable     = 360.0
	headingUnavailable = 511
	latUnavailable     = 91.0
	lonUnavailable     = 181.0
)

// timeLayout matches MetaData.time_utc, e.g. "2023-06-11 16:13:44.452496606 +0000 UTC".
const timeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

// Decode parses one message. Unknown types return (nil, nil).
func Decode(raw []byte, receivedAt time.Time) (*Decoded, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode AIS envelope: %w", err)
	}
	body, ok := env.Message[env.MessageType]
	if !ok {
		return nil, nil
	}
	observed := metaTime(env.MetaData, receivedAt)

	switch env.MessageType {
	case TypePositionReport, TypeStandardClassBPosition, TypeExtendedClassBPosition:
		var p position
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("decode %s: %w", env.MessageType, err)
		}
		if !p.Valid || p.UserID <= 0 || p.Latitude == latUnavailable || p.Longitude == lonUnavailable {
			return nil, nil
		}
		mmsi := strconv.FormatInt(p.UserID, 10)
		f := domain.Frame{
			EntityKey: domain.AISKey(mmsi), Domain: domain.DomainSea, Source: domain.SourceAISStream,
			ObservedAt: observed, ReceivedAt: receivedAt,
			Lat: p.Latitude, Lon: p.Longitude, MMSI: mmsi, AISClass: "B",
		}
		if env.MessageType == TypePositionReport {
			f.AISClass = "A"
			f.NavStatus = p.NavigationalStatus
		}
		if p.Sog < sogUnavailable {
			f.GroundSpeedMps = domain.Ptr(p.Sog * domain.KnotToMps)
		}
		if p.Cog < cogUnavailable {
			f.TrackDeg = domain.Ptr(p.Cog)
		}
		if p.TrueHeading != headingUnavailable && p.TrueHeading >= 0 && p.TrueHeading < 360 {
			f.HeadingDeg = domain.Ptr(float64(p.TrueHeading))
		}
		d := &Decoded{MMSI: mmsi, Frame: &f}
		if env.MessageType == TypeExtendedClassBPosition {
			st := Static{Name: domain.SanitizeIdent(p.Name), ShipType: p.Type}
			st.LengthM, st.BeamM = dims(p.Dimension)
			d.Static = &st
		}
		return d, nil

	case TypeShipStaticData:
		var s shipStatic
		if err := json.Unmarshal(body, &s); err != nil {
			return nil, fmt.Errorf("decode %s: %w", env.MessageType, err)
		}
		if !s.Valid || s.UserID <= 0 {
			return nil, nil
		}
		st := Static{Name: domain.SanitizeIdent(s.Name), CallSign: domain.SanitizeIdent(s.CallSign), ShipType: s.Type}
		if s.ImoNumber > 0 {
			st.IMO = strconv.FormatInt(s.ImoNumber, 10)
		}
		st.LengthM, st.BeamM = dims(s.Dimension)
		return &Decoded{MMSI: strconv.FormatInt(s.UserID, 10), Static: &st}, nil

	case TypeStaticDataReport:
		var s staticReport
		if err := json.Unmarshal(body, &s); err != nil {
			return nil, fmt.Errorf("decode %s: %w", env.MessageType, err)
		}
		if !s.Valid || s.UserID <= 0 {
			return nil, nil
		}
		var st Static
		if s.ReportA.Valid {
			st.Name = domain.SanitizeIdent(s.ReportA.Name)
		}
		if s.ReportB.Valid {
			st.CallSign = domain.SanitizeIdent(s.ReportB.CallSign)
			if s.ReportB.ShipType > 0 {
				st.ShipType = domain.Ptr(s.ReportB.ShipType)
			}
			st.LengthM, st.BeamM = dims(s.ReportB.Dimension)
		}
		return &Decoded{MMSI: strconv.FormatInt(s.UserID, 10), Static: &st}, nil
	}
	return nil, nil
}

func dims(d *dimension) (length, beam *float64) {
	if d == nil {
		return nil, nil
	}
	if l := d.A + d.B; l > 0 {
		length = domain.Ptr(float64(l))
	}
	if b := d.C + d.D; b > 0 {
		beam = domain.Ptr(float64(b))
	}
	return length, beam
}

func metaTime(meta map[string]json.RawMessage, fallback time.Time) time.Time {
	raw, ok := meta["time_utc"]
	if !ok {
		return fallback
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fallback
	}
	t, err := time.Parse(timeLayout, strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return t
}

// ApplyStatic joins cached static data onto a position frame.
func ApplyStatic(f *domain.Frame, s Static) {
	f.ShipName = s.Name
	f.ShipCall = s.CallSign
	f.IMO = s.IMO
	f.ShipType = s.ShipType
	f.LengthM = s.LengthM
	f.BeamM = s.BeamM
}
