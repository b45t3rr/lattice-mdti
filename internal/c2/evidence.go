// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package c2

import (
	"fmt"
	"math"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/viz"
)

// speedOfSoundMps is the ISA sea-level speed of sound, used only to express
// implied speeds as a Mach number for the operator.
const speedOfSoundMps = 340.3

// Evidence states, in operator terms, the physical inconsistency Tier 1
// measured, and returns the map layers that show it. It reads only the
// anomaly's own metrics and window, never attacker-controlled identifiers.
func Evidence(ev domain.AnomalyEvent) (headline string, layers []viz.Layer) {
	m := ev.Metrics
	n := len(ev.Window)
	switch ev.Type {
	case domain.AnomalyKinematicImpossible:
		v := m["implied_speed_mps"]
		if jump, dt := m["jump_m"], m["dt_s"]; jump > 0 && dt > 0 {
			headline = fmt.Sprintf("IMPOSSIBLE KINEMATICS · %.1f km in %.0f s · %s m/s (Mach %.0f)",
				jump/1000, dt, thousands(v), v/speedOfSoundMps)
		} else {
			headline = fmt.Sprintf("IMPOSSIBLE KINEMATICS · implied %s m/s (Mach %.1f)", thousands(v), v/speedOfSoundMps)
		}
		if n >= 2 {
			a, b := ev.Window[n-2], ev.Window[n-1]
			layers = append(layers, viz.Layer{ID: "mdti-evidence-jump-" + ev.EntityID, Name: headline,
				Description: "Consecutive position reports of the same transponder: no aircraft can cover this distance in this time.",
				Shape:       viz.Line([]geo.Point{{Lat: a.Lat, Lon: a.Lon}, {Lat: b.Lat, Lon: b.Lon}}),
				Style:       viz.Style{Line: viz.Red}})
		}
	case domain.AnomalyBaroGNSSDivergence:
		dh := m["delta_alt_ft"]
		switch {
		case m["deviation_ft"] != 0:
			headline = fmt.Sprintf("GNSS/BARO MISMATCH · GNSS altitude %+.0f ft from baro (%+.0f ft off its baseline)", dh, m["deviation_ft"])
		case m["rate_ft_s"] != 0:
			headline = fmt.Sprintf("GNSS/BARO MISMATCH · %+.0f ft, drifting %.0f ft/s", dh, m["rate_ft_s"])
		default:
			headline = fmt.Sprintf("GNSS/BARO MISMATCH · GNSS altitude %+.0f ft from baro", dh)
		}
		if n > 0 && ev.Window[n-1].NACp != nil {
			headline += fmt.Sprintf(" · NACp %d", *ev.Window[n-1].NACp)
		}
	case domain.AnomalyGhostTrack:
		dist := m["dist_ref_nm"]
		if dist == 0 {
			dist = ev.Geo.DistToAerodromeNM
		}
		headline = fmt.Sprintf("GHOST TRACK · first seen %.1f NM from %s, airborne, no track history",
			dist, nonEmpty(ev.Geo.NearestAerodrome, "the protected aerodrome"))
		if n > 0 {
			f := ev.Window[0]
			layers = append(layers, viz.Layer{ID: "mdti-evidence-spawn-" + ev.EntityID, Name: "FIRST FIX · appeared here with no history",
				Description: "A real aircraft enters coverage from its edge or takes off from a runway; this one appeared mid-coverage.",
				Shape:       viz.Point(geo.Point{Lat: f.Lat, Lon: f.Lon}), Style: viz.Style{Type: Lattice.GeoDetailsTypeGeoTypeHazard, Line: viz.Red}})
		}
	case domain.AnomalyAISDarkEEZ:
		gap := time.Duration(m["gap_s"] * float64(time.Second)).Round(time.Second)
		where := "near the 200 NM line"
		if d := m["signed_dist_nm"]; d < 0 {
			where = fmt.Sprintf("%.1f NM inside the 200 NM line", -d)
		} else if d > 0 {
			where = fmt.Sprintf("%.1f NM outside the 200 NM line", d)
		}
		headline = fmt.Sprintf("AIS SILENT %s · last report %s", gap, where)
		if m["dr_inside"] > 0 {
			headline += " · dead-reckoned position now inside the EEZ"
		}
	case domain.AnomalyNavIntegrityDrop:
		headline = "NAV INTEGRITY DROP · NIC/NACp collapsed"
	default:
		headline = string(ev.Type)
	}
	return headline, layers
}

func thousands(v float64) string {
	s := fmt.Sprintf("%.0f", math.Round(v))
	out := ""
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += ","
		}
		out += string(c)
	}
	return out
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
