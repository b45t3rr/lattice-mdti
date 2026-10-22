// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package tier1

import (
	"math"
	"slices"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// teleport records one R2 jump for oscillation analysis.
type teleport struct {
	at      time.Time
	bearing float64
}

// hit is one rule firing on one frame (or one dark-vessel check).
type hit struct {
	typ     domain.AnomalyType
	sev     int
	rule    string
	metrics map[string]float64
}

func pt(f domain.Frame) geo.Point { return geo.Point{Lat: f.Lat, Lon: f.Lon} }

// ---------------------------------------------------------------------------
// D1 — kinematic impossibility (SPEC §6.3)

func (e *Engine) d1Kinematic(t *track, prev, cur domain.Frame) []hit {
	c := &e.cfg
	dt := cur.ObservedAt.Sub(prev.ObservedAt).Seconds()
	d := geo.Haversine(pt(prev), pt(cur))
	vmax := c.vmax(cur.Domain == domain.DomainSea, cur.Category)
	var hits []hit

	// R2 teleport: even sub-0.5 s pairs count (a hijacked ICAO's real and
	// injected reports can arrive a fraction of a second apart), with Δt
	// floored so jitter cannot fabricate speed.
	dtEff := math.Max(dt, c.MinPairDt.Seconds())
	allow := vmax*(dtEff+c.TimingSlack.Seconds()) + 2*math.Max(sigmaPos(prev.NACp), sigmaPos(cur.NACp))
	if d > allow {
		v := d / dtEff
		sev := 3
		if v > 3*vmax {
			sev = 4
		}
		t.teleports = append(t.teleports, teleport{at: cur.ObservedAt, bearing: geo.Bearing(pt(prev), pt(cur))})
		hits = append(hits, hit{domain.AnomalyKinematicImpossible, sev, "D1.R2 teleport",
			map[string]float64{"jump_m": d, "dt_s": dt, "implied_speed_mps": v, "vmax_mps": vmax, "allowed_m": allow}})
		// R5 oscillation: jumps back and forth between two positions, the
		// signature of a re-broadcast ICAO interleaving with the real aircraft.
		// A monotonic overspeed run is not an oscillation.
		cut := cur.ObservedAt.Add(-c.OscillationWin)
		t.teleports = slices.DeleteFunc(t.teleports, func(x teleport) bool { return x.at.Before(cut) })
		reversals := 0
		for i := 1; i < len(t.teleports); i++ {
			if math.Abs(geo.AngleDiff(t.teleports[i-1].bearing, t.teleports[i].bearing)) > 120 {
				reversals++
			}
		}
		if reversals >= c.OscillationN-1 {
			hits = append(hits, hit{domain.AnomalyKinematicImpossible, 4, "D1.R5 oscillation",
				map[string]float64{"jumps_in_window": float64(len(t.teleports)), "reversals": float64(reversals)}})
		}
	}

	if dt < c.MinPairDt.Seconds() {
		return hits
	}
	// R1/R3/R4 work over ≥ SpeedBaseline: aggregator timestamps carry ±0.5 s
	// of error and velocity fields can be stale for several seconds (observed
	// live on a take-off roll: 81 kt repeated for 5 s, then 109 kt 0.6 s later).
	base, ok := baselineFrame(t.frames, cur, c.SpeedBaseline)
	if !ok {
		return hits
	}
	span := cur.ObservedAt.Sub(base.ObservedAt).Seconds()
	// R4 longitudinal acceleration from the aircraft's reported ground speed.
	if cur.GroundSpeedMps != nil && base.GroundSpeedMps != nil {
		if a := math.Abs(*cur.GroundSpeedMps-*base.GroundSpeedMps) / span; a > c.MaxAccel {
			hits = append(hits, hit{domain.AnomalyKinematicImpossible, 2, "D1.R4 acceleration",
				map[string]float64{"accel_mps2": a, "max_mps2": c.MaxAccel, "span_s": span}})
		}
	}
	v := geo.Haversine(pt(base), pt(cur)) / span
	// R1 overspeed, sustained over two evaluations.
	if v > vmax {
		t.overspeedRun++
	} else {
		t.overspeedRun = 0
	}
	if t.overspeedRun >= 2 {
		hits = append(hits, hit{domain.AnomalyKinematicImpossible, 3, "D1.R1 overspeed",
			map[string]float64{"implied_speed_mps": v, "vmax_mps": vmax}})
	}
	// R3 reported (mean over the span) vs implied speed.
	if gs, ok := meanGS(t.frames, base.ObservedAt); ok {
		if resid := math.Abs(v - gs); resid > math.Max(c.SpeedResidual, c.SpeedResidualPc*gs) {
			t.residualRun++
			if t.residualRun >= 2 {
				hits = append(hits, hit{domain.AnomalyKinematicImpossible, 2, "D1.R3 speed residual",
					map[string]float64{"implied_speed_mps": v, "reported_speed_mps": gs, "residual_mps": resid, "span_s": span}})
			}
		} else {
			t.residualRun = 0
		}
	}
	return hits
}

// baselineFrame returns the newest frame at least minSpan older than cur.
// frames ends with cur.
func baselineFrame(frames []domain.Frame, cur domain.Frame, minSpan time.Duration) (domain.Frame, bool) {
	for i := len(frames) - 2; i >= 0; i-- {
		if cur.ObservedAt.Sub(frames[i].ObservedAt) >= minSpan {
			return frames[i], true
		}
	}
	return domain.Frame{}, false
}

// meanGS averages reported ground speed over frames observed at or after from.
func meanGS(frames []domain.Frame, from time.Time) (float64, bool) {
	sum, n := 0.0, 0
	for _, f := range frames {
		if !f.ObservedAt.Before(from) && f.GroundSpeedMps != nil {
			sum += *f.GroundSpeedMps
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// ---------------------------------------------------------------------------
// D2 — Baro/GNSS divergence (SPEC §6.4)

func deltaH(f domain.Frame) (float64, bool) {
	if f.AltBaroFt == nil || f.AltGeomFt == nil || (f.OnGround != nil && *f.OnGround) {
		return 0, false
	}
	return *f.AltGeomFt - *f.AltBaroFt, true
}

func (e *Engine) d2BaroGNSS(t *track, prev *domain.Frame, cur domain.Frame) []hit {
	c := &e.cfg
	dh, ok := deltaH(cur)
	if !ok {
		return nil
	}
	var hits []hit
	// R2 absolute divergence (needs no baseline).
	if math.Abs(dh) > c.BaroAbsFt {
		hits = append(hits, hit{domain.AnomalyBaroGNSSDivergence, 3, "D2.R2 absolute",
			map[string]float64{"delta_alt_ft": dh, "threshold_ft": c.BaroAbsFt}})
	}
	// R1 step against the per-track baseline (median, excluding the newest K).
	var hist []float64
	for _, f := range t.frames[:len(t.frames)-1] {
		if v, ok := deltaH(f); ok {
			hist = append(hist, v)
		}
	}
	k := c.BaroStepFrames
	if len(hist)-(k-1) >= c.BaroMinBaseline {
		base := median(hist[:len(hist)-(k-1)])
		if dev := math.Abs(dh - base); dev > c.BaroStepFt {
			t.stepRun++
			if t.stepRun >= k {
				hits = append(hits, hit{domain.AnomalyBaroGNSSDivergence, 3, "D2.R1 step",
					map[string]float64{"delta_alt_ft": dh, "baseline_ft": base, "deviation_ft": dev}})
			}
		} else {
			t.stepRun = 0
		}
	}
	// R3 divergence rate.
	if prev != nil {
		if dhPrev, ok := deltaH(*prev); ok {
			if dt := cur.ObservedAt.Sub(prev.ObservedAt).Seconds(); dt >= c.MinPairDt.Seconds() {
				if rate := math.Abs(dh-dhPrev) / dt; rate > c.BaroRateFtS {
					t.rateRun++
					if t.rateRun >= 2 {
						hits = append(hits, hit{domain.AnomalyBaroGNSSDivergence, 3, "D2.R3 rate",
							map[string]float64{"delta_alt_ft": dh, "rate_ft_s": rate}})
					}
				} else {
					t.rateRun = 0
				}
			}
		}
	}
	// R4 vertical-rate divergence.
	if cur.VertRateGeomFpm != nil && cur.VertRateBaroFpm != nil {
		if div := math.Abs(*cur.VertRateGeomFpm - *cur.VertRateBaroFpm); div > c.BaroRateDivFpm {
			t.divRun++
			if t.divRun >= 3 {
				hits = append(hits, hit{domain.AnomalyBaroGNSSDivergence, 2, "D2.R4 rate divergence",
					map[string]float64{"vrate_div_fpm": div}})
			}
		} else {
			t.divRun = 0
		}
	}
	return hits
}

func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// ---------------------------------------------------------------------------
// D3 — ADS-B ghost (SPEC §6.5). Evaluated once, on the GhostFrames-th frame.

func (e *Engine) d3Ghost(t *track) []hit {
	c := &e.cfg
	first := t.frames[0]
	p := pt(first)
	if first.OnGround != nil && *first.OnGround || first.AltBaroFt == nil {
		return nil
	}
	var ref geo.Aerodrome
	refDist := math.Inf(1)
	for _, icao := range c.GhostRefs {
		if a, ok := e.aerodromes[icao]; ok {
			if d := geo.Haversine(p, a.Point()); d < refDist {
				ref, refDist = a, d
			}
		}
	}
	if refDist > c.GhostRadiusNM*domain.NMToM {
		return nil
	}
	agl := *first.AltBaroFt - ref.ElevFt
	if agl <= c.GhostMinAGLFt {
		return nil
	}
	edge := c.CoverageRadiusNM - geo.Haversine(c.CoverageCenter, p)/domain.NMToM
	if edge <= c.GhostEdgeNM {
		return nil // entered from outside the coverage circle: not a spawn
	}
	for _, a := range e.aerodromes {
		if geo.Haversine(p, a.Point()) <= c.GhostRunwayNM*domain.NMToM && *first.AltBaroFt-a.ElevFt < c.GhostRunwayMaxAGL {
			return nil // plausible departure
		}
	}
	sev := 2
	boost := 0.0
	if first.Callsign == "" {
		sev, boost = sev+1, boost+1
	}
	if first.NACp == nil || *first.NACp <= 4 {
		sev, boost = sev+1, boost+1
	}
	switch first.Squawk {
	case "7500", "7600", "7700":
		sev, boost = sev+1, boost+1
	}
	return []hit{{domain.AnomalyGhostTrack, min(sev, 4), "D3 ghost spawn", map[string]float64{
		"dist_ref_nm": refDist / domain.NMToM, "alt_agl_ft": agl, "coverage_edge_nm": edge, "boosters": boost,
	}}}
}

// ---------------------------------------------------------------------------
// D4 — EEZ dark vessel (SPEC §6.6). Driven by the clock, not by frames.

// expectedInterval implements the ITU-R M.1371 reporting intervals.
func expectedInterval(f domain.Frame) time.Duration {
	if f.AISClass == "B" {
		return 30 * time.Second
	}
	if f.NavStatus != nil && (*f.NavStatus == 1 || *f.NavStatus == 5) {
		return 180 * time.Second // at anchor / moored
	}
	if f.GroundSpeedMps == nil {
		return 10 * time.Second
	}
	switch kn := *f.GroundSpeedMps / domain.KnotToMps; {
	case kn > 23:
		return 2 * time.Second
	case kn > 14:
		return 6 * time.Second
	case kn < 0.5:
		return 180 * time.Second
	default:
		return 10 * time.Second
	}
}

func (e *Engine) d4Dark(t *track, now time.Time) []hit {
	c := &e.cfg
	if e.zones == nil || t.darkFlagged || len(t.frames) == 0 || t.fix == nil {
		return nil
	}
	last := t.frames[len(t.frames)-1]
	gap := now.Sub(last.ObservedAt)
	threshold := max(c.DarkMinGap, time.Duration(c.DarkGapFactor*float64(expectedInterval(last))))
	if gap <= threshold || math.Abs(t.fix.SignedDistM) > c.EEZBandNM*domain.NMToM {
		return nil
	}
	dr := pt(last)
	inwardAngle := math.NaN()
	if last.TrackDeg != nil && last.GroundSpeedMps != nil {
		dr = geo.DeadReckon(pt(last), *last.TrackDeg, *last.GroundSpeedMps, gap.Seconds())
		inwardAngle = math.Abs(geo.AngleDiff(*last.TrackDeg, t.fix.InwardBearingDeg))
	}
	inside := e.zones.Contains(dr) != nil
	if !inside && !(inwardAngle < c.InwardAngleDeg) {
		return nil
	}
	t.darkFlagged = true

	sev := 3
	course := 0.0
	if last.TrackDeg != nil {
		for _, f := range t.frames[:len(t.frames)-1] {
			if f.TrackDeg != nil && last.ObservedAt.Sub(f.ObservedAt) <= c.CourseAlterWin {
				course = math.Max(course, math.Abs(geo.AngleDiff(*f.TrackDeg, *last.TrackDeg)))
			}
		}
	}
	if course > c.CourseAlterDeg {
		sev++
	}
	if last.ShipType != nil && *last.ShipType == 30 {
		sev++
	}
	// Coverage is judged up to the vessel's last report: its own silence since
	// then is what we are trying to explain, so counting it would be circular
	// (found live: an hour boundary turned prior 0.00 into 0.50).
	prior := e.coverage.prior(pt(last), last.ObservedAt)
	if prior > c.CoveragePriorHigh {
		sev--
	}
	m := map[string]float64{
		"gap_s": gap.Seconds(), "threshold_s": threshold.Seconds(),
		"signed_dist_nm": t.fix.SignedDistM / domain.NMToM, "dr_lat": dr.Lat, "dr_lon": dr.Lon,
		"dr_inside": b2f(inside), "course_change_deg": course, "coverage_prior": prior,
	}
	if !math.IsNaN(inwardAngle) {
		m["inward_angle_deg"] = inwardAngle
	}
	return []hit{{domain.AnomalyAISDarkEEZ, max(1, min(sev, 4)), "D4 dark vessel", m}}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// D5 — navigation integrity drop (SPEC §6.7)

func (e *Engine) d5Integrity(t *track, cur domain.Frame) []hit {
	c := &e.cfg
	if cur.OnGround != nil && *cur.OnGround {
		return nil
	}
	check := func(get func(domain.Frame) *int, name string) *hit {
		v := get(cur)
		if v == nil || *v > c.IntegrityFloor {
			return nil
		}
		peak := -1
		for _, f := range t.frames[:len(t.frames)-1] {
			if p := get(f); p != nil && cur.ObservedAt.Sub(f.ObservedAt) <= c.IntegrityWin && *p > peak {
				peak = *p
			}
		}
		if peak < 0 || peak-*v < c.IntegrityDrop {
			return nil
		}
		return &hit{domain.AnomalyNavIntegrityDrop, 2, "D5 " + name + " drop",
			map[string]float64{name: float64(*v), name + "_before": float64(peak)}}
	}
	var hits []hit
	if h := check(func(f domain.Frame) *int { return f.NACp }, "nac_p"); h != nil {
		hits = append(hits, *h)
	}
	if h := check(func(f domain.Frame) *int { return f.NIC }, "nic"); h != nil {
		hits = append(hits, *h)
	}
	return hits
}
