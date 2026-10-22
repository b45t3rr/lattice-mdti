// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package geo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/planar"
	"github.com/paulmach/orb/simplify"
	"gopkg.in/yaml.v3"
)

// LegalStatus annotates an unmodified official polygon (SPEC §6.6.1).
type LegalStatus string

const (
	SovereignEEZ    LegalStatus = "SOVEREIGN_EEZ"
	DisputedClaimed LegalStatus = "DISPUTED_CLAIMED"
	JointRegime     LegalStatus = "JOINT_REGIME"
)

// ZonePolicy is extra escalation policy for a zone, enforced by Tier 2 guardrails.
type ZonePolicy struct {
	MaxAutonomousTask       string `yaml:"max_autonomous_task" json:"max_autonomous_task,omitempty"`
	HostileRequiresOperator bool   `yaml:"hostile_requires_operator" json:"hostile_requires_operator,omitempty"`
}

// Zone is one monitored maritime area.
type Zone struct {
	ID          string      `yaml:"id"`
	Name        string      `yaml:"name"`
	File        string      `yaml:"file"`
	SHA256      string      `yaml:"sha256"`
	MRGID       int         `yaml:"mrgid"`
	LegalStatus LegalStatus `yaml:"legal_status"`
	Monitored   bool        `yaml:"monitored"`
	Policy      ZonePolicy  `yaml:"policy"`

	geom  orb.MultiPolygon
	bound orb.Bound
}

type registryFile struct {
	Dataset struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
		License string `yaml:"license"`
	} `yaml:"dataset"`
	SimplifyToleranceDeg float64 `yaml:"simplify_tolerance_deg"`
	OuterLimits          struct {
		File    string `yaml:"file"`
		SHA256  string `yaml:"sha256"`
		LineIDs []int  `yaml:"line_ids"`
	} `yaml:"outer_limits"`
	Zones []*Zone `yaml:"zones"`
}

type segment struct {
	a, b           Point
	minLat, maxLat float64
}

// ZoneSet answers geometric questions against the monitored zones and the
// 200 NM outer limit lines (the "mile 200" boundary with the high seas).
type ZoneSet struct {
	Zones []*Zone
	outer []segment
}

// ZoneFix is the result of a position query.
type ZoneFix struct {
	Zone *Zone // containing monitored zone, nil if outside all of them
	// SignedDistM is the distance to the nearest 200 NM outer limit, negative
	// when inside a monitored zone (SPEC §6.6.2).
	SignedDistM float64
	// NearestOuter is the closest point on the outer limit lines.
	NearestOuter Point
	// InwardBearingDeg points from the position towards the inside of the
	// monitored area across the nearest outer limit.
	InwardBearingDeg float64
}

// LoadZones reads the registry YAML, verifies file hashes, and builds the
// simplified geometry. Paths in the registry are relative to its directory.
func LoadZones(registryPath string) (*ZoneSet, error) {
	raw, err := os.ReadFile(registryPath)
	if err != nil {
		return nil, err
	}
	var reg registryFile
	if err := yaml.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", registryPath, err)
	}
	dir := filepath.Dir(registryPath)
	tol := reg.SimplifyToleranceDeg
	if tol <= 0 {
		tol = 0.001 // ≈ 100 m
	}
	dp := simplify.DouglasPeucker(tol)

	zs := &ZoneSet{}
	for _, z := range reg.Zones {
		fc, err := readVerified(filepath.Join(dir, z.File), z.SHA256)
		if err != nil {
			return nil, fmt.Errorf("zone %s: %w", z.ID, err)
		}
		for _, f := range fc.Features {
			switch g := f.Geometry.(type) {
			case orb.Polygon:
				z.geom = append(z.geom, g)
			case orb.MultiPolygon:
				z.geom = append(z.geom, g...)
			default:
				return nil, fmt.Errorf("zone %s: unsupported geometry %T", z.ID, g)
			}
		}
		z.geom = dp.MultiPolygon(z.geom)
		z.bound = z.geom.Bound()
		zs.Zones = append(zs.Zones, z)
	}

	fc, err := readVerified(filepath.Join(dir, reg.OuterLimits.File), reg.OuterLimits.SHA256)
	if err != nil {
		return nil, fmt.Errorf("outer limits: %w", err)
	}
	want := map[int]bool{}
	for _, id := range reg.OuterLimits.LineIDs {
		want[id] = true
	}
	for _, f := range fc.Features {
		if id, ok := f.Properties["line_id"].(float64); !ok || !want[int(id)] {
			continue
		}
		var lines orb.MultiLineString
		switch g := f.Geometry.(type) {
		case orb.LineString:
			lines = orb.MultiLineString{g}
		case orb.MultiLineString:
			lines = g
		default:
			return nil, fmt.Errorf("outer limits: unsupported geometry %T", g)
		}
		for _, ls := range dp.MultiLineString(lines) {
			for i := 1; i < len(ls); i++ {
				a, b := Point{Lat: ls[i-1][1], Lon: ls[i-1][0]}, Point{Lat: ls[i][1], Lon: ls[i][0]}
				zs.outer = append(zs.outer, segment{a: a, b: b, minLat: math.Min(a.Lat, b.Lat), maxLat: math.Max(a.Lat, b.Lat)})
			}
		}
	}
	if len(zs.outer) == 0 {
		return nil, fmt.Errorf("outer limits: no segments for line ids %v", reg.OuterLimits.LineIDs)
	}
	return zs, nil
}

func readVerified(path, wantSHA string) (*geojson.FeatureCollection, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); wantSHA == "" || got != wantSHA {
		return nil, fmt.Errorf("%s: sha256 mismatch (got %s)", filepath.Base(path), got)
	}
	return geojson.UnmarshalFeatureCollection(raw)
}

// Contains returns the first monitored zone containing p, or nil.
func (zs *ZoneSet) Contains(p Point) *Zone {
	pt := orb.Point{p.Lon, p.Lat}
	for _, z := range zs.Zones {
		if z.Monitored && z.bound.Contains(pt) && planar.MultiPolygonContains(z.geom, pt) {
			return z
		}
	}
	return nil
}

// Fix computes containment, signed distance to the outer limit and the inward
// bearing for p.
func (zs *ZoneSet) Fix(p Point) ZoneFix {
	best, bestPt := math.Inf(1), Point{}
	for _, s := range zs.outer {
		// Latitude-only lower bound lets us skip far segments cheaply.
		var dLat float64
		if p.Lat < s.minLat {
			dLat = s.minLat - p.Lat
		} else if p.Lat > s.maxLat {
			dLat = p.Lat - s.maxLat
		}
		if rad(dLat)*EarthRadiusM >= best {
			continue
		}
		if d, q := distToSegment(p, s.a, s.b); d < best {
			best, bestPt = d, q
		}
	}
	fix := ZoneFix{Zone: zs.Contains(p), SignedDistM: best, NearestOuter: bestPt}
	toBoundary := Bearing(p, bestPt)
	if fix.Zone != nil {
		fix.SignedDistM = -best
		fix.InwardBearingDeg = NormalizeDeg(toBoundary + 180)
	} else {
		fix.InwardBearingDeg = toBoundary
	}
	return fix
}

// distToSegment returns the distance from p to segment ab (local ENU around p)
// and the closest point.
func distToSegment(p, a, b Point) (float64, Point) {
	ax, ay := ENU(p, a)
	bx, by := ENU(p, b)
	dx, dy := bx-ax, by-ay
	t := 0.0
	if l2 := dx*dx + dy*dy; l2 > 0 {
		t = math.Max(0, math.Min(1, -(ax*dx+ay*dy)/l2))
	}
	cx, cy := ax+t*dx, ay+t*dy
	q := Point{Lat: a.Lat + t*(b.Lat-a.Lat), Lon: a.Lon + t*AngleDiff(a.Lon, b.Lon)}
	return math.Hypot(cx, cy), q
}

// ByID returns the zone with the given id, or nil.
func (zs *ZoneSet) ByID(id string) *Zone {
	for _, z := range zs.Zones {
		if z.ID == id {
			return z
		}
	}
	return nil
}
