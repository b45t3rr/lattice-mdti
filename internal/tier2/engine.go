// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package tier2

import (
	"container/heap"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"golang.org/x/time/rate"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// DecisionsAPI is the subset of the OpenAI client Tier 2 uses.
type DecisionsAPI interface {
	New(ctx context.Context, body openai.DecisionNewParams, opts ...option.RequestOption) (*openai.Decision, error)
}

// Result is one decided anomaly with the material needed for evidence.
type Result struct {
	Event      domain.AnomalyEvent
	Decision   domain.TacticalDecision
	Evidence   []byte // the exact evidence JSON sent to the model (nil on cache/fallback)
	RawAnswers string
}

// Engine schedules escalated anomalies onto the Decisions API.
type Engine struct {
	API         DecisionsAPI
	Policy      *Policy
	Model       string
	Autonomy    string
	Zones       *geo.ZoneSet
	Concurrency int
	PerMinute   int
	QueueSize   int
	Timeout     time.Duration
	SafetyID    string
	Log         *slog.Logger
	Now         func() time.Time

	mu      sync.Mutex
	cond    *sync.Cond
	queue   eventHeap
	cache   map[cacheKey]cached
	applied map[string]appliedDisp // entity → last authorised disposition (flap protection)
	limiter *rate.Limiter

	Calls, Fallbacks, CacheHits, Dropped atomic.Uint64
}

type cacheKey struct {
	entity string
	typ    domain.AnomalyType
}

type cached struct {
	d  domain.TacticalDecision
	at time.Time
}

type appliedDisp struct {
	disp domain.Disposition
	at   time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) init() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cond != nil {
		return
	}
	e.cond = sync.NewCond(&e.mu)
	e.cache = map[cacheKey]cached{}
	e.applied = map[string]appliedDisp{}
	per := max(1, e.PerMinute)
	e.limiter = rate.NewLimiter(rate.Every(time.Minute/time.Duration(per)), 1)
	if e.QueueSize <= 0 {
		e.QueueSize = 64
	}
	if e.Timeout <= 0 {
		e.Timeout = 10 * time.Second
	}
}

// Submit queues an escalated anomaly. When the queue is full the lowest
// severity (then oldest) event is dropped (SPEC §7.1).
func (e *Engine) Submit(ev domain.AnomalyEvent) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	heap.Push(&e.queue, ev)
	if e.queue.Len() > e.QueueSize {
		dropped := e.queue.removeLowest()
		e.Dropped.Add(1)
		e.Log.Warn("Tier-2 queue full, dropping lowest severity", "entity", dropped.EntityID, "severity", dropped.Severity)
	}
	e.cond.Signal()
}

func (e *Engine) pop(ctx context.Context) (domain.AnomalyEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.queue.Len() == 0 {
		if ctx.Err() != nil {
			return domain.AnomalyEvent{}, false
		}
		e.cond.Wait()
	}
	return heap.Pop(&e.queue).(domain.AnomalyEvent), true
}

// Run consumes anomalies, decides escalated ones and emits results until ctx ends.
func (e *Engine) Run(ctx context.Context, in <-chan domain.AnomalyEvent, out chan<- Result) {
	e.init()
	go func() { <-ctx.Done(); e.mu.Lock(); e.cond.Broadcast(); e.mu.Unlock() }()
	var wg sync.WaitGroup
	for range max(1, e.Concurrency) {
		wg.Go(func() {
			for {
				ev, ok := e.pop(ctx)
				if !ok {
					return
				}
				r := e.Decide(ctx, ev)
				select {
				case out <- r:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case ev, ok := <-in:
			if !ok {
				wg.Wait()
				return
			}
			if ev.Escalate {
				e.Submit(ev)
			}
		}
	}
}

// Decide produces a decision for one anomaly: cache, then the Decisions API,
// then deterministic policy; any failure falls back (SPEC §7.9).
func (e *Engine) Decide(ctx context.Context, ev domain.AnomalyEvent) Result {
	e.init()
	dctx := Context{Autonomy: e.Autonomy, Now: e.now(), ZonePolicy: e.zonePolicy(ev)}
	key := cacheKey{ev.EntityID, ev.Type}

	e.mu.Lock()
	c, hit := e.cache[key]
	e.mu.Unlock()
	if hit && dctx.Now.Sub(c.at) < e.Policy.CacheTTL {
		e.CacheHits.Add(1)
		d := c.d
		d.AnomalyID = ev.ID
		d.Audit.Cached, d.Audit.DecidedAt, d.Audit.LatencyMs = true, dctx.Now, 0
		d.Audit.GuardrailsApplied = nil
		e.Policy.authorize(&d, dctx)
		e.flapGuard(&d, dctx.Now)
		return Result{Event: ev, Decision: d}
	}

	params, evidence, err := BuildParams(e.Model, ev, e.SafetyID)
	if err != nil {
		return e.fallback(ev, "evidence: "+err.Error(), dctx)
	}
	if err := e.limiter.Wait(ctx); err != nil {
		return e.fallback(ev, "rate limiter: "+err.Error(), dctx)
	}
	callCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	start := time.Now()
	e.Calls.Add(1)
	resp, err := e.API.New(callCtx, params, option.WithMaxRetries(2))
	latency := time.Since(start)
	if err != nil {
		return e.fallback(ev, "decisions API: "+err.Error(), dctx)
	}
	answers, err := ParseAnswers(resp)
	if err != nil {
		return e.fallback(ev, err.Error(), dctx)
	}
	d := e.Policy.Decide(ev, answers, resp.Model, dctx)
	d.Audit.LatencyMs = latency.Milliseconds()
	e.flapGuard(&d, dctx.Now)

	e.mu.Lock()
	e.cache[key] = cached{d: d, at: dctx.Now}
	for k, v := range e.cache {
		if dctx.Now.Sub(v.at) > e.Policy.CacheTTL {
			delete(e.cache, k)
		}
	}
	e.mu.Unlock()
	return Result{Event: ev, Decision: d, Evidence: evidence, RawAnswers: resp.RawJSON()}
}

func (e *Engine) fallback(ev domain.AnomalyEvent, reason string, dctx Context) Result {
	e.Fallbacks.Add(1)
	e.Log.Warn("Tier-2 fallback", "entity", ev.EntityID, "reason", reason)
	d := e.Policy.Fallback(ev, reason, dctx)
	e.flapGuard(&d, dctx.Now)
	return Result{Event: ev, Decision: d}
}

// flapGuard blocks disposition changes within FlapProtection unless escalating.
func (e *Engine) flapGuard(d *domain.TacticalDecision, now time.Time) {
	if d.Action.Disposition == "" {
		return
	}
	rank := map[domain.Disposition]int{domain.DispositionNeutral: 0, domain.DispositionSuspect: 1, domain.DispositionHostile: 2}
	e.mu.Lock()
	defer e.mu.Unlock()
	prev, ok := e.applied[d.EntityID]
	if ok && now.Sub(prev.at) < e.Policy.FlapProtection && rank[d.Action.Disposition] <= rank[prev.disp] {
		if d.Action.Disposition != prev.disp {
			d.Audit.GuardrailsApplied = append(d.Audit.GuardrailsApplied, "flap_protection")
		}
		d.Action.Disposition = "" // nothing new to apply
		return
	}
	e.applied[d.EntityID] = appliedDisp{disp: d.Action.Disposition, at: now}
}

func (e *Engine) zonePolicy(ev domain.AnomalyEvent) *geo.ZonePolicy {
	if e.Zones == nil || ev.Geo.Zone == nil {
		return nil
	}
	if z := e.Zones.ByID(ev.Geo.Zone.ID); z != nil {
		return &z.Policy
	}
	return nil
}

// ErrNotConfigured is returned when OPENAI_API_KEY is missing.
var ErrNotConfigured = errors.New("OPENAI_API_KEY is not set")

// eventHeap orders by severity (desc) then detection time (asc).
type eventHeap []domain.AnomalyEvent

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].Severity != h[j].Severity {
		return h[i].Severity > h[j].Severity
	}
	return h[i].DetectedAt.Before(h[j].DetectedAt)
}
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)   { *h = append(*h, x.(domain.AnomalyEvent)) }
func (h *eventHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// removeLowest removes and returns the lowest-priority element.
func (h *eventHeap) removeLowest() domain.AnomalyEvent {
	worst := 0
	for i := 1; i < h.Len(); i++ {
		if h.Less(worst, i) {
			worst = i
		}
	}
	return heap.Remove(h, worst).(domain.AnomalyEvent)
}
