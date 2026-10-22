// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package ing

import (
	"context"
	"log/slog"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

// Engine owns the per-track lifecycle: first-seen time, the latest frame, and
// dead-reckoning (DR) mode for vessels that went dark (SPEC §5.4). All state is
// confined to the Run goroutine.
type Engine struct {
	Mapper     *Mapper
	Publisher  *Publisher
	Log        *slog.Logger
	DRInterval time.Duration // DR publish cadence
	DRMax      time.Duration // give up DR after this long
	EvictAfter time.Duration // forget tracks not seen for this long (non-DR)
	Now        func() time.Time

	tracks   map[string]*track
	drReq    chan drRequest
	classReq chan classification
}

// classification is C2's verdict on a track. Lattice only allows overriding
// mil_view.disposition, so symbology and the operator note are carried by
// ING's own publishes (verified live, SPEC §8.1).
type classification struct {
	entityID string
	disp     Lattice.MilViewDisposition // "" keeps the published symbology
	note     string
}

type track struct {
	created time.Time
	last    domain.Frame
	drSince time.Time // zero when not in DR mode
	class   *classification
}

type drRequest struct {
	entityID string
	on       bool
}

// NewEngine builds an engine with SPEC defaults.
func NewEngine(m *Mapper, p *Publisher, log *slog.Logger) *Engine {
	return &Engine{
		Mapper: m, Publisher: p, Log: log.With("module", "ing"),
		DRInterval: 30 * time.Second, DRMax: 2 * time.Hour, EvictAfter: 30 * time.Minute,
		drReq: make(chan drRequest, 64), classReq: make(chan classification, 64),
	}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// StartDeadReckoning switches a track into DR mode (called by C2 when an
// AIS_DARK_EEZ anomaly is confirmed). Safe from any goroutine.
func (e *Engine) StartDeadReckoning(entityID string) { e.drReq <- drRequest{entityID, true} }

// StopDeadReckoning leaves DR mode (e.g. an agent completed its task).
func (e *Engine) StopDeadReckoning(entityID string) { e.drReq <- drRequest{entityID, false} }

// Classify records C2's verdict: the SIDC affiliation follows disp and the
// description is prefixed with note on every subsequent publish (and one is
// published immediately). Safe from any goroutine.
func (e *Engine) Classify(entityID string, disp Lattice.MilViewDisposition, note string) {
	e.classReq <- classification{entityID: entityID, disp: disp, note: note}
}

// Run consumes frames until in is closed or ctx is cancelled.
func (e *Engine) Run(ctx context.Context, in <-chan domain.Frame) {
	if e.tracks == nil {
		e.tracks = map[string]*track{}
	}
	tick := time.NewTicker(min(e.DRInterval, time.Minute))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-in:
			if !ok {
				return
			}
			e.handle(f)
		case r := <-e.drReq:
			e.setDR(r)
		case c := <-e.classReq:
			e.setClass(c)
		case <-tick.C:
			e.housekeeping()
		}
	}
}

func (e *Engine) handle(f domain.Frame) {
	id := domain.EntityID(f.EntityKey)
	t, ok := e.tracks[id]
	if !ok {
		t = &track{created: e.now()}
		e.tracks[id] = t
	}
	if !t.last.ObservedAt.IsZero() && !f.ObservedAt.After(t.last.ObservedAt) {
		return // out-of-order or duplicate (e.g. hijacked tracks interleaving)
	}
	if !t.drSince.IsZero() {
		e.Log.Info("dark vessel reappeared, leaving dead-reckoning", "entity", id,
			"dark_for", f.ObservedAt.Sub(t.last.ObservedAt).Round(time.Second))
		t.drSince = time.Time{}
	}
	t.last = f
	e.Publisher.Submit(e.overlay(e.Mapper.Entity(f, t.created), t))
}

func (e *Engine) setClass(c classification) {
	t, ok := e.tracks[c.entityID]
	if !ok {
		e.Log.Warn("classification for unknown track", "entity", c.entityID)
		return
	}
	t.class = &c
	if t.drSince.IsZero() {
		// Republish at once, 1 ms after the last report: Lattice needs a newer
		// update time, and using "now" would make the next (lagging) real
		// reports look stale.
		f := t.last
		f.ObservedAt = f.ObservedAt.Add(time.Millisecond)
		e.Publisher.Submit(e.overlay(e.Mapper.Entity(f, t.created), t))
	} else {
		e.publishDR(c.entityID, t)
	}
}

// overlay applies C2's classification to an outgoing entity.
func (e *Engine) overlay(ent *Lattice.Entity, t *track) *Lattice.Entity {
	if t.class == nil {
		return ent
	}
	if t.class.disp != "" && ent.Symbology != nil && ent.Symbology.MilStd2525C != nil && ent.Symbology.MilStd2525C.Sidc != nil {
		ent.Symbology.MilStd2525C.Sidc = Lattice.String(lattice.WithAffiliation(*ent.Symbology.MilStd2525C.Sidc, t.class.disp))
	}
	if t.class.note != "" {
		desc := t.class.note
		if ent.Description != nil && *ent.Description != "" {
			desc += " · " + *ent.Description
		}
		ent.Description = Lattice.String(truncateRunes(desc, 1024))
	}
	return ent
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (e *Engine) setDR(r drRequest) {
	t, ok := e.tracks[r.entityID]
	if !ok {
		e.Log.Warn("dead-reckoning requested for unknown track", "entity", r.entityID)
		return
	}
	switch {
	case r.on && t.drSince.IsZero():
		t.drSince = e.now()
		e.Log.Info("entering dead-reckoning", "entity", r.entityID, "last_report", t.last.ObservedAt)
		e.publishDR(r.entityID, t)
	case !r.on && !t.drSince.IsZero():
		t.drSince = time.Time{}
		e.Log.Info("leaving dead-reckoning", "entity", r.entityID)
	}
}

func (e *Engine) housekeeping() {
	now := e.now()
	for id, t := range e.tracks {
		switch {
		case !t.drSince.IsZero() && now.Sub(t.drSince) > e.DRMax:
			e.Log.Info("dead-reckoning expired", "entity", id, "after", e.DRMax)
			t.drSince = time.Time{}
		case !t.drSince.IsZero():
			e.publishDR(id, t)
		case now.Sub(t.last.ObservedAt) > e.EvictAfter:
			delete(e.tracks, id)
			e.Publisher.Forget(id)
		}
	}
}

// DeadReckon projects the last report forward to now along COG at SOG. Without
// a course or speed the last position is kept.
func DeadReckon(last domain.Frame, now time.Time) geo.Point {
	p := geo.Point{Lat: last.Lat, Lon: last.Lon}
	if last.TrackDeg == nil || last.GroundSpeedMps == nil {
		return p
	}
	return geo.DeadReckon(p, *last.TrackDeg, *last.GroundSpeedMps, now.Sub(last.ObservedAt).Seconds())
}

func (e *Engine) publishDR(id string, t *track) {
	now := e.now()
	pos := DeadReckon(t.last, now)
	f := t.last
	f.Lat, f.Lon = pos.Lat, pos.Lon
	f.ObservedAt = now // SourceUpdateTime must advance for Lattice to apply it
	ent := e.Mapper.Entity(f, t.created)
	ent.ExpiryTime = ptr(now.Add(DRExpiry))
	ent.Tracked.TrackQualityWrapper = Lattice.Int(1)
	ent.Tracked.LastMeasurementTime = ptr(t.last.ObservedAt)
	ent.Description = Lattice.String("AIS DARK – dead-reckoned since " +
		t.last.ObservedAt.UTC().Format("15:04:05Z") + " · " + *ent.Description)
	e.Publisher.Submit(e.overlay(ent, t))
}
