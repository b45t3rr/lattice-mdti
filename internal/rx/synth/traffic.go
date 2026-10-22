// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package synth generates benign background traffic for the offline demo:
// airliners crossing the ADS-B coverage area and vessels on fixed lanes in
// the Río de la Plata and off Patagonia. Every identity is marked synthetic
// (callsigns and ship names start with "SYN"), the kinematics are physically
// consistent, and none of it should ever trip a Tier-1 detector: it is the
// control group that shows MDTI ignores normal traffic.
package synth

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// Traffic is a deterministic background-traffic source.
type Traffic struct {
	Center   geo.Point // ADS-B coverage centre
	RadiusNM float64   // ADS-B coverage radius
	Aircraft int
	Seed     uint64
	// Avoid keeps initial aircraft positions away from these points (e.g. the
	// ghost-watch aerodrome) so the first fix never looks like a spawn there.
	Avoid   []geo.Point
	AvoidNM float64
	Now     func() time.Time

	rng *rand.Rand
	st  *state
}

type plane struct {
	key, callsign, squawk, typ string
	pos                        geo.Point
	trackDeg, speedMps         float64
	altBaroFt, geoOffsetFt     float64
}

type vessel struct {
	key, mmsi, name string
	shipType, nav   int
	lengthM         float64
	a, b            geo.Point // lane end points
	pos             geo.Point
	speedMps        float64
	toB             bool
	every           time.Duration
	next            time.Time
}

// lanes are sea routes (all water) with the vessel types that use them.
var lanes = []struct {
	a, b     geo.Point
	shipType int
	label    string
	speedMps float64
}{
	{geo.Point{Lat: -34.66, Lon: -58.22}, geo.Point{Lat: -35.02, Lon: -56.55}, 70, "CARGO", 6.5},
	{geo.Point{Lat: -34.78, Lon: -57.95}, geo.Point{Lat: -35.25, Lon: -56.90}, 80, "TANKER", 5.5},
	{geo.Point{Lat: -34.70, Lon: -57.70}, geo.Point{Lat: -34.98, Lon: -57.10}, 70, "CARGO", 6.0},
	{geo.Point{Lat: -45.95, Lon: -67.10}, geo.Point{Lat: -46.60, Lon: -63.50}, 70, "CARGO", 6.5},
	{geo.Point{Lat: -45.60, Lon: -62.50}, geo.Point{Lat: -46.90, Lon: -61.20}, 30, "FISHER", 4.0},
	{geo.Point{Lat: -46.00, Lon: -60.30}, geo.Point{Lat: -45.30, Lon: -61.40}, 30, "FISHER", 3.5},
	{geo.Point{Lat: -46.70, Lon: -60.10}, geo.Point{Lat: -47.20, Lon: -61.60}, 30, "FISHER", 4.5},
}

var types = []string{"A320", "B738", "E190", "A321", "B763"}

func (t *Traffic) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Traffic) radiusM() float64 { return t.RadiusNM * domain.NMToM }

// spawn creates an aircraft on the coverage edge heading across it, or (at
// start-up) somewhere along that crossing.
func (t *Traffic) spawn(n int, initial bool) *plane {
	r := t.rng
	brg := r.Float64() * 360
	p := &plane{
		key:      domain.ADSBKey(fmt.Sprintf("e2%04x", r.IntN(0x10000))),
		callsign: fmt.Sprintf("SYN%03d", 100+n),
		squawk:   fmt.Sprintf("%d%d%d%d", 1+r.IntN(6), r.IntN(8), r.IntN(8), r.IntN(8)),
		typ:      types[r.IntN(len(types))],
		pos:      geo.Destination(t.Center, brg, 0.97*t.radiusM()),
		trackDeg: geo.NormalizeDeg(brg + 180 + (r.Float64()-0.5)*70),
		speedMps: 130 + r.Float64()*110,
	}
	if r.IntN(3) == 0 {
		p.altBaroFt = 6000 + float64(r.IntN(16))*500 // regional / approach
	} else {
		p.altBaroFt = 24000 + float64(r.IntN(14))*1000 // en route
	}
	p.geoOffsetFt = 200 + r.Float64()*250
	if initial {
		for range 20 {
			q := geo.Destination(p.pos, p.trackDeg, r.Float64()*1.6*t.radiusM())
			if geo.Haversine(t.Center, q) < 0.95*t.radiusM() && t.clear(q) {
				p.pos = q
				break
			}
		}
	}
	return p
}

func (t *Traffic) clear(q geo.Point) bool {
	for _, a := range t.Avoid {
		if geo.Haversine(a, q) < t.AvoidNM*domain.NMToM {
			return false
		}
	}
	return true
}

func (t *Traffic) vessels(start time.Time) []*vessel {
	var out []*vessel
	for i, l := range lanes {
		for j := range 2 {
			n := i*2 + j
			frac := t.rng.Float64()
			v := &vessel{
				mmsi: fmt.Sprintf("70199%04d", n+1), name: fmt.Sprintf("SYN %s %d", l.label, n+1),
				shipType: l.shipType, lengthM: map[int]float64{30: 42, 70: 180, 80: 230}[l.shipType],
				a: l.a, b: l.b, toB: j == 0, speedMps: l.speedMps * (0.9 + 0.2*t.rng.Float64()),
				every: 10 * time.Second, next: start.Add(time.Duration(t.rng.IntN(10)) * time.Second),
			}
			if l.shipType == 30 {
				v.nav = 7 // engaged in fishing
			}
			v.key = domain.AISKey(v.mmsi)
			v.pos = geo.Destination(l.a, geo.Bearing(l.a, l.b), frac*geo.Haversine(l.a, l.b))
			out = append(out, v)
		}
	}
	return out
}

func (p *plane) frame(at time.Time) domain.Frame {
	return domain.Frame{
		EntityKey: p.key, Domain: domain.DomainAir, Source: domain.SourceSynthetic, ObservedAt: at, ReceivedAt: at,
		Lat: p.pos.Lat, Lon: p.pos.Lon, GroundSpeedMps: domain.Ptr(p.speedMps), TrackDeg: domain.Ptr(p.trackDeg),
		AltBaroFt: domain.Ptr(roundTo(p.altBaroFt, 25)), AltGeomFt: domain.Ptr(roundTo(p.altBaroFt+p.geoOffsetFt, 25)),
		VertRateBaroFpm: domain.Ptr(0.0), VertRateGeomFpm: domain.Ptr(0.0), OnGround: domain.Ptr(false),
		Callsign: p.callsign, Squawk: p.squawk, Category: "A3", AircraftType: p.typ,
		NIC: domain.Ptr(8), NACp: domain.Ptr(9), SIL: domain.Ptr(3),
	}
}

func (v *vessel) frame(at time.Time) domain.Frame {
	brg := geo.Bearing(v.a, v.b)
	if !v.toB {
		brg = geo.Bearing(v.b, v.a)
	}
	return domain.Frame{
		EntityKey: v.key, Domain: domain.DomainSea, Source: domain.SourceSynthetic, ObservedAt: at, ReceivedAt: at,
		Lat: v.pos.Lat, Lon: v.pos.Lon, GroundSpeedMps: domain.Ptr(v.speedMps), TrackDeg: domain.Ptr(brg), HeadingDeg: domain.Ptr(brg),
		MMSI: v.mmsi, ShipName: v.name, ShipType: domain.Ptr(v.shipType), NavStatus: domain.Ptr(v.nav), AISClass: "A",
		LengthM: domain.Ptr(v.lengthM),
	}
}

// step advances a vessel, reversing at the lane ends.
func (v *vessel) step(dt time.Duration) {
	to := v.b
	if !v.toB {
		to = v.a
	}
	d := v.speedMps * dt.Seconds()
	if rem := geo.Haversine(v.pos, to); rem <= d {
		v.pos, v.toB = to, !v.toB
		return
	}
	v.pos = geo.Destination(v.pos, geo.Bearing(v.pos, to), d)
}

func roundTo(x, q float64) float64 { return float64(int64(x/q+0.5)) * q }

// state is the running simulation (set by Start).
type state struct {
	planes  []*plane
	ships   []*vessel
	spawned int
	last    time.Time
}

// Start initialises the traffic at time start.
func (t *Traffic) Start(start time.Time) {
	t.rng = rand.New(rand.NewPCG(t.Seed, 0x5e7))
	st := &state{planes: make([]*plane, t.Aircraft), spawned: t.Aircraft, last: start}
	for i := range st.planes {
		st.planes[i] = t.spawn(i, true)
	}
	st.ships = t.vessels(start)
	t.st = st
}

// Step advances the traffic to now and returns the frames reported: every
// aircraft, and the vessels whose report interval has elapsed.
func (t *Traffic) Step(now time.Time) []domain.Frame {
	st := t.st
	dt := now.Sub(st.last)
	st.last = now
	var out []domain.Frame
	for i, p := range st.planes {
		p.pos = geo.Destination(p.pos, p.trackDeg, p.speedMps*dt.Seconds())
		if geo.Haversine(t.Center, p.pos) > t.radiusM() {
			st.planes[i] = t.spawn(st.spawned, false)
			st.spawned++
			p = st.planes[i]
		}
		out = append(out, p.frame(now))
	}
	for _, v := range st.ships {
		v.step(dt)
		if !now.Before(v.next) {
			v.next = now.Add(v.every)
			out = append(out, v.frame(now))
		}
	}
	return out
}

// Run emits frames at 1 Hz until ctx ends.
func (t *Traffic) Run(ctx context.Context, out chan<- domain.Frame) error {
	t.Start(t.now())
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		for _, f := range t.Step(t.now()) {
			if !emit(ctx, out, f) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func emit(ctx context.Context, out chan<- domain.Frame, f domain.Frame) bool {
	select {
	case out <- f:
		return true
	case <-ctx.Done():
		return false
	}
}
