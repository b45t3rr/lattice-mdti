// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package viz

import (
	"fmt"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// Rendering notes (verified on the Lattice C2, 2026-10-22): ellipses,
// polygons and lines render with their line colour; fills do not render;
// hazard points render as an icon. Sensor fields of view, ActiveTarget
// relationships and points of interest with media do not render, so MDTI
// draws sensor rings and intercept lines as geo entities instead.
//
// Track panel (cmd/validate panel-probe, 2026-10-22): Status.PlatformActivity
// is shown as "Activity" (MDTI puts the evidence headline there) and
// Indicators.Simulated as a "Simulated" badge. Description, Status.Role,
// Media, Health.ActiveAlerts and VisualDetails.RangeRings are not shown.
// TargetPriority renders an "HVT/HPT" label: deliberately unused, targeting
// semantics are outside MDTI's defensive scope.

// Overlays are the static background layers of the MDTI picture.
func Overlays(zones *geo.ZoneSet, aerodromes geo.Aerodromes, coverageCenter geo.Point, coverageNM float64, ghostNM float64) []Layer {
	var ls []Layer
	if sadm, ok := aerodromes["SADM"]; ok {
		shape, c := Circle(sadm.Point(), ghostNM*domain.NMToM)
		ls = append(ls, Layer{ID: "mdti-geo-sadm-watch", Name: fmt.Sprintf("MORON (SADM) · ghost watch %0.f NM", ghostNM),
			Description: "Tier-1 D3: a new airborne identity spawning inside this ring with no history is a ghost-track candidate.",
			Shape:       shape, Center: c, Style: Style{Line: Orange}})
	}
	if coverageNM > 0 {
		shape, c := Circle(coverageCenter, coverageNM*domain.NMToM)
		ls = append(ls, Layer{ID: "mdti-geo-adsb-coverage", Name: fmt.Sprintf("ADS-B coverage · %0.f NM query", coverageNM),
			Description: "Area polled from the adsb.fi aggregator.", Shape: shape, Center: c, Style: Style{Line: Grey}})
	}
	if zones != nil {
		for i, l := range zones.OuterPolylines(400) {
			name, color := "EEZ 200 NM limit (Milla 201)", Cyan
			if l.LineID != zones.PrimaryLineID {
				name, color = "200 NM limit · disputed (AR claim)", Orange
			}
			ls = append(ls, Layer{ID: fmt.Sprintf("mdti-geo-eez200-%d", i), Name: name,
				Description: "Marine Regions EEZ v12 outer limit. Tier-1 D4 watches vessels within 12 NM of it.",
				Shape:       Line(l.Points), Style: Style{Line: color}})
		}
	}
	return ls
}

// Alert is the marker C2 draws where an anomaly was confirmed: an area ring
// and a hazard point carrying the label.
func Alert(entityID string, at geo.Point, label, description string, color RGBA) []Layer {
	shape, c := Circle(at, 2*domain.NMToM)
	return []Layer{
		{ID: "mdti-alert-area-" + entityID, Name: label, Description: description, Shape: shape, Center: c,
			Style: Style{Type: Lattice.GeoDetailsTypeGeoTypeHazard, Line: color}},
		{ID: "mdti-alert-" + entityID, Name: label, Description: description, Shape: Point(geo.Destination(at, 0, 2*domain.NMToM)),
			Style: Style{Type: Lattice.GeoDetailsTypeGeoTypeHazard, Line: color}},
	}
}

// SearchArea is the RF emitter search area for an Investigate-on-point task.
func SearchArea(entityID string, at geo.Point, radiusM float64) Layer {
	shape, c := Circle(at, radiusM)
	return Layer{ID: "mdti-rf-search-" + entityID, Name: "RF emitter search area · " + entityID,
		Description: "GNSS spoofing suspected: the emitter is searched around the affected track.",
		Shape:       shape, Center: c, Style: Style{Line: Yellow}}
}

// DeadReckoning draws a dark vessel's last report and its estimated track.
func DeadReckoning(entityID string, last, now geo.Point, lastAt time.Time) []Layer {
	return []Layer{
		{ID: "mdti-dr-track-" + entityID, Name: "AIS DARK · dead-reckoned track",
			Description: "Estimated path since the last AIS report (constant course and speed).",
			Shape:       Line([]geo.Point{last, now}), Style: Style{Line: Orange}},
		{ID: "mdti-dr-off-" + entityID, Name: "AIS OFF · last report " + lastAt.UTC().Format("15:04Z"),
			Shape: Point(last), Style: Style{Type: Lattice.GeoDetailsTypeGeoTypeHazard, Line: Orange}},
	}
}

// SensorRing is the asset's EO/IR identification range, moving with it.
func SensorRing(assetID, name string, at geo.Point, rangeM float64) Layer {
	return Layer{ID: "mdti-sensor-" + assetID, Name: name + " · EO/IR range",
		Shape: CirclePolygon(at, rangeM, 36), Style: Style{Line: Green}}
}

// InterceptLine joins an asset to its target.
func InterceptLine(assetID, name string, from, to geo.Point, label string) Layer {
	return Layer{ID: "mdti-intercept-" + assetID, Name: name + " · " + label,
		Shape: Line([]geo.Point{from, to}), Style: Style{Line: Yellow}}
}

// PatrolStation is the orbit an asset holds when idle (demo profile).
func PatrolStation(assetID, name string, at geo.Point, radiusM float64) Layer {
	shape, c := Circle(at, radiusM)
	return Layer{ID: "mdti-station-" + assetID, Name: name + " · patrol station", Shape: shape, Center: c,
		Style: Style{Line: Cyan.WithAlpha(0.6)}}
}
