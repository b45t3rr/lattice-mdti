// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package tier1

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// track is the per-entity sliding window and detector state.
type track struct {
	key, id   string
	domain    domain.Domain
	frames    []domain.Frame // oldest first, ≤ WindowSize, Source stripped
	firstSeen time.Time

	ghostEligible bool
	ghostChecked  bool

	overspeedRun, residualRun int
	teleports                 []teleport
	stepRun, rateRun, divRun  int

	fix         *geo.ZoneFix // SEA: zone fix of the latest frame
	darkFlagged bool

	fired    map[domain.AnomalyType]time.Time // last time each type fired (corroboration)
	lastEmit map[domain.AnomalyType]emitted   // cooldown
}

type emitted struct {
	at  time.Time
	sev int
}

// Engine runs the five detectors over a frame stream. It is single-threaded
// and deterministic: the same frames produce the same events. Not safe for
// concurrent use; Run owns it in the live pipeline.
type Engine struct {
	cfg        Config
	zones      *geo.ZoneSet
	aerodromes geo.Aerodromes
	coverage   *coverageGrid

	tracks    map[string]*track
	recent    map[string]time.Time // evicted entity → eviction time (ghost guard)
	start     time.Time
	startAir  time.Time // receipt time of the first AIR frame: the ghost warm-up reference
	clock     time.Time
	lastCheck time.Time
	seq       uint64

	Frames, Ignored uint64
}

// New builds an engine. zones may be nil (D4 disabled).
func New(cfg Config, zones *geo.ZoneSet, aerodromes geo.Aerodromes) *Engine {
	return &Engine{
		cfg: cfg, zones: zones, aerodromes: aerodromes, coverage: newCoverageGrid(),
		tracks: map[string]*track{}, recent: map[string]time.Time{},
	}
}

// Clock returns the engine's stream time.
func (e *Engine) Clock() time.Time { return e.clock }

// Process ingests one frame and returns any events it triggers.
func (e *Engine) Process(f domain.Frame) []domain.AnomalyEvent {
	f = f.Strip()
	if e.start.IsZero() {
		e.start = f.ObservedAt
		e.lastCheck = f.ObservedAt
	}
	// Warm-up is measured on receipt time: observation times lag by up to a
	// minute (aggregator seen_pos) and AIS time_utc by more.
	received := f.ReceivedAt
	if received.IsZero() {
		received = f.ObservedAt
	}
	if f.Domain == domain.DomainAir && e.startAir.IsZero() {
		e.startAir = received
	}
	events := e.Advance(f.ObservedAt)
	e.Frames++
	id := domain.EntityID(f.EntityKey)
	t, ok := e.tracks[id]
	if !ok {
		_, wasRecent := e.recent[id]
		t = &track{
			key: f.EntityKey, id: id, domain: f.Domain, firstSeen: f.ObservedAt,
			ghostEligible: f.Domain == domain.DomainAir && !wasRecent && received.Sub(e.startAir) >= e.cfg.GhostWarmup,
			fired:         map[domain.AnomalyType]time.Time{}, lastEmit: map[domain.AnomalyType]emitted{},
		}
		e.tracks[id] = t
		delete(e.recent, id)
	}
	var prev *domain.Frame
	if n := len(t.frames); n > 0 {
		last := t.frames[n-1]
		if !f.ObservedAt.After(last.ObservedAt) {
			e.Ignored++
			return events // duplicate or out of order
		}
		if f.Domain == domain.DomainAir && f.ObservedAt.Sub(last.ObservedAt) > e.cfg.ResetGap {
			t.frames = t.frames[:0]
			t.overspeedRun, t.residualRun, t.stepRun, t.rateRun, t.divRun = 0, 0, 0, 0, 0
		} else {
			prev = &last
		}
	}
	t.frames = append(t.frames, f)
	if len(t.frames) > e.cfg.WindowSize {
		t.frames = slices.Delete(t.frames, 0, len(t.frames)-e.cfg.WindowSize)
	}

	var hits []hit
	switch f.Domain {
	case domain.DomainAir:
		if prev != nil {
			hits = append(hits, e.d1Kinematic(t, *prev, f)...)
		}
		hits = append(hits, e.d5Integrity(t, f)...)
		hits = append(hits, e.d2BaroGNSS(t, prev, f)...)
		if t.ghostEligible && !t.ghostChecked && len(t.frames) >= e.cfg.GhostFrames {
			t.ghostChecked = true
			hits = append(hits, e.d3Ghost(t)...)
		}
	case domain.DomainSea:
		e.coverage.observe(pt(f), f.ObservedAt)
		t.darkFlagged = false
		if e.zones != nil {
			fix := e.zones.Fix(pt(f))
			t.fix = &fix
		}
		if prev != nil {
			hits = append(hits, e.d1Kinematic(t, *prev, f)...)
		}
	}
	if ev, ok := e.emit(t, hits, f.ObservedAt); ok {
		events = append(events, ev)
	}
	return events
}

// Advance moves the stream clock forward (never back) and runs clock-driven
// checks: dark vessels and eviction.
func (e *Engine) Advance(now time.Time) []domain.AnomalyEvent {
	if !now.After(e.clock) {
		return nil
	}
	e.clock = now
	if e.lastCheck.IsZero() {
		e.lastCheck = now
	}
	if now.Sub(e.lastCheck) < e.cfg.DarkCheckEvery {
		return nil
	}
	e.lastCheck = now
	var events []domain.AnomalyEvent
	for _, id := range slices.Sorted(maps.Keys(e.tracks)) { // sorted: deterministic output
		t := e.tracks[id]
		if t.domain == domain.DomainSea {
			if ev, ok := e.emit(t, e.d4Dark(t, now), now); ok {
				events = append(events, ev)
			}
		}
		if len(t.frames) > 0 && now.Sub(t.frames[len(t.frames)-1].ObservedAt) > e.cfg.EvictAfter && !t.darkFlagged {
			delete(e.tracks, id)
			e.recent[id] = now
		}
	}
	for id, at := range e.recent {
		if now.Sub(at) > e.cfg.GhostRecentExpiry {
			delete(e.recent, id)
		}
	}
	return events
}

// emit merges this frame's hits into at most one event, applying
// corroboration, severity adjustment and cooldown (SPEC §6.8).
func (e *Engine) emit(t *track, hits []hit, now time.Time) (domain.AnomalyEvent, bool) {
	if len(hits) == 0 {
		return domain.AnomalyEvent{}, false
	}
	for _, h := range hits {
		t.fired[h.typ] = now
	}
	recently := func(typ domain.AnomalyType) bool {
		at, ok := t.fired[typ]
		return ok && now.Sub(at) <= e.cfg.Corroborate
	}
	// D2 is raised by one when navigation integrity also dropped.
	for i := range hits {
		if hits[i].typ == domain.AnomalyBaroGNSSDivergence && recently(domain.AnomalyNavIntegrityDrop) {
			hits[i].sev = min(4, hits[i].sev+1)
		}
	}
	top := hits[0]
	for _, h := range hits[1:] {
		if h.sev > top.sev {
			top = h
		}
	}
	var corr []domain.AnomalyType
	for typ := range t.fired {
		if typ != top.typ && recently(typ) {
			corr = append(corr, typ)
		}
	}
	slices.Sort(corr)

	if last, ok := t.lastEmit[top.typ]; ok && now.Sub(last.at) < e.cfg.Cooldown && top.sev <= last.sev {
		return domain.AnomalyEvent{}, false
	}
	t.lastEmit[top.typ] = emitted{at: now, sev: top.sev}
	for _, c := range corr {
		if _, ok := t.lastEmit[c]; !ok || now.Sub(t.lastEmit[c].at) >= e.cfg.Cooldown {
			t.lastEmit[c] = emitted{at: now, sev: top.sev}
		}
	}

	e.seq++
	window := slices.Clone(t.frames)
	ev := domain.AnomalyEvent{
		ID:            fmt.Sprintf("%013x-%06x", now.UnixMilli(), e.seq),
		EntityID:      t.id,
		EntityKey:     t.key,
		Domain:        t.domain,
		Type:          top.typ,
		Severity:      top.sev,
		Rule:          top.rule,
		Metrics:       top.metrics,
		Corroborating: corr,
		Escalate:      top.sev >= 3 || (top.sev == 2 && len(corr) > 0),
		Window:        window,
		Geo:           e.geoContext(t, now),
		DetectedAt:    now,
	}
	return ev, true
}

func (e *Engine) geoContext(t *track, now time.Time) domain.GeoContext {
	var g domain.GeoContext
	if len(t.frames) == 0 {
		return g
	}
	p := pt(t.frames[len(t.frames)-1])
	if len(e.aerodromes) > 0 {
		a, d := e.aerodromes.Nearest(p)
		g.NearestAerodrome, g.DistToAerodromeNM = a.ICAO, d/domain.NMToM
	}
	switch t.domain {
	case domain.DomainAir:
		edge := e.cfg.CoverageRadiusNM - geo.Haversine(e.cfg.CoverageCenter, p)/domain.NMToM
		g.CoverageEdgeDistNM = &edge
	case domain.DomainSea:
		if t.fix != nil {
			s := t.fix.SignedDistM / domain.NMToM
			inside := t.fix.Zone != nil
			g.SignedDistToEEZNM, g.InsideEEZ = &s, &inside
			if t.fix.Zone != nil {
				g.Zone = &domain.ZoneRef{ID: t.fix.Zone.ID, LegalStatus: string(t.fix.Zone.LegalStatus)}
			}
		}
		prior := e.coverage.prior(p, t.frames[len(t.frames)-1].ObservedAt) // see d4Dark
		g.AISCoveragePrior = &prior
	}
	return g
}

// Run drives the engine from a live frame channel. With wallClock, a ticker
// advances the clock to wall time so silence (dark vessels) is detected even
// when no frames arrive; disable it for as-fast-as-possible replays.
func (e *Engine) Run(ctx context.Context, in <-chan domain.Frame, out chan<- domain.AnomalyEvent, wallClock bool, log *slog.Logger) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	send := func(evs []domain.AnomalyEvent) {
		for _, ev := range evs {
			log.Info("anomaly", "entity", ev.EntityID, "type", ev.Type, "severity", ev.Severity, "rule", ev.Rule,
				"escalate", ev.Escalate, "corroborating", ev.Corroborating)
			select {
			case out <- ev:
			case <-ctx.Done():
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-in:
			if !ok {
				return
			}
			send(e.Process(f))
		case <-tick.C:
			if wallClock && !e.start.IsZero() {
				send(e.Advance(time.Now()))
			}
		}
	}
}
