// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package ing

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/option"
	"golang.org/x/time/rate"

	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

// EntityAPI is the subset of the SDK entities client the publisher needs.
type EntityAPI interface {
	PublishEntity(ctx context.Context, request *Lattice.Entity, opts ...option.RequestOption) (*Lattice.Entity, error)
}

// Publisher coalesces entity updates (latest wins per entity id), flushes them
// on a fixed cadence through a token bucket, and adapts its rate to 429s with
// AIMD (SPEC §5.3).
type Publisher struct {
	API      EntityAPI
	MaxRate  float64       // publishes per second
	Workers  int           // concurrent HTTP calls
	Interval time.Duration // flush cadence
	Log      *slog.Logger

	mu      sync.Mutex
	pending map[string]*Lattice.Entity
	last    map[string]time.Time // entity id → last SourceUpdateTime sent
	limiter *rate.Limiter
	backoff time.Time // while in the future, the rate is reduced

	Published   atomic.Uint64
	Failed      atomic.Uint64
	Rejected    atomic.Uint64 // 4xx other than 429: the entity itself is invalid
	RateLimited atomic.Uint64
	Superseded  atomic.Uint64 // replaced in the queue before being sent
	Stale       atomic.Uint64 // older than what was already sent
}

// NewPublisher builds a publisher with SPEC defaults.
func NewPublisher(api EntityAPI, maxRate float64, log *slog.Logger) *Publisher {
	if maxRate <= 0 {
		maxRate = 40
	}
	return &Publisher{API: api, MaxRate: maxRate, Workers: 4, Interval: time.Second, Log: log}
}

func (p *Publisher) init() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = map[string]*Lattice.Entity{}
		p.last = map[string]time.Time{}
		p.limiter = rate.NewLimiter(rate.Limit(p.MaxRate), max(1, int(p.MaxRate)))
	}
}

// Submit queues an entity. An update whose SourceUpdateTime is not newer than
// the queued or already-sent version is ignored (Lattice would ignore it too).
func (p *Publisher) Submit(e *Lattice.Entity) {
	p.init()
	id := *e.EntityID
	t := updateTime(e)
	p.mu.Lock()
	defer p.mu.Unlock()
	if sent, ok := p.last[id]; ok && !t.After(sent) {
		p.Stale.Add(1)
		return
	}
	if q, ok := p.pending[id]; ok {
		if !t.After(updateTime(q)) {
			p.Stale.Add(1)
			return
		}
		p.Superseded.Add(1)
	}
	p.pending[id] = e
}

// Pending returns the number of queued entities.
func (p *Publisher) Pending() int {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

// Run flushes until ctx is cancelled, then makes one final flush with a short
// deadline.
func (p *Publisher) Run(ctx context.Context) {
	p.init()
	tick := time.NewTicker(p.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			p.Flush(drain)
			cancel()
			return
		case <-tick.C:
			p.Flush(ctx)
		}
	}
}

// Flush publishes everything queued right now.
func (p *Publisher) Flush(ctx context.Context) {
	p.init()
	p.mu.Lock()
	batch := make([]*Lattice.Entity, 0, len(p.pending))
	for id, e := range p.pending {
		batch = append(batch, e)
		delete(p.pending, id)
	}
	p.recoverRate()
	p.mu.Unlock()
	if len(batch) == 0 {
		return
	}

	work := make(chan *Lattice.Entity)
	var wg sync.WaitGroup
	for range max(1, p.Workers) {
		wg.Go(func() {
			for e := range work {
				p.publish(ctx, e)
			}
		})
	}
	for _, e := range batch {
		if err := p.limiter.Wait(ctx); err != nil {
			p.requeue(e)
			continue
		}
		work <- e
	}
	close(work)
	wg.Wait()
}

func (p *Publisher) publish(ctx context.Context, e *Lattice.Entity) {
	_, err := p.API.PublishEntity(ctx, e)
	if err == nil {
		p.Published.Add(1)
		p.mu.Lock()
		if t := updateTime(e); t.After(p.last[*e.EntityID]) {
			p.last[*e.EntityID] = t
		}
		p.mu.Unlock()
		return
	}
	switch code := lattice.StatusCode(err); {
	case code == http.StatusTooManyRequests:
		p.RateLimited.Add(1)
		p.throttle()
		p.requeue(e)
	case code >= 400 && code < 500:
		// The entity is invalid (usually a wrong enum literal or field): retrying
		// cannot help. Log the server's message, which names the field.
		p.Rejected.Add(1)
		p.Log.Error("entity rejected by Lattice", "entity", label(e), "err", lattice.Describe(err))
	default:
		p.Failed.Add(1)
		if ctx.Err() == nil {
			p.Log.Warn("entity publish failed, will retry", "entity", label(e), "err", lattice.Describe(err))
			p.requeue(e)
		}
	}
}

// requeue puts e back unless a newer version arrived meanwhile.
func (p *Publisher) requeue(e *Lattice.Entity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if q, ok := p.pending[*e.EntityID]; !ok || updateTime(e).After(updateTime(q)) {
		p.pending[*e.EntityID] = e
	}
}

// throttle halves the rate for 30 s (multiplicative decrease).
func (p *Publisher) throttle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := float64(p.limiter.Limit())
	next := max(1, cur/2)
	p.limiter.SetLimit(rate.Limit(next))
	p.backoff = time.Now().Add(30 * time.Second)
	p.Log.Warn("Lattice rate limited, slowing down", "rate", next)
}

// recoverRate adds 1/s per flush after the backoff window (additive increase).
// Caller holds p.mu.
func (p *Publisher) recoverRate() {
	if time.Now().Before(p.backoff) {
		return
	}
	if cur := float64(p.limiter.Limit()); cur < p.MaxRate {
		p.limiter.SetLimit(rate.Limit(min(p.MaxRate, cur+1)))
	}
}

// Forget drops per-entity bookkeeping (call when a track is evicted).
func (p *Publisher) Forget(id string) {
	p.init()
	p.mu.Lock()
	delete(p.last, id)
	p.mu.Unlock()
}

func updateTime(e *Lattice.Entity) time.Time {
	if e.Provenance != nil && e.Provenance.SourceUpdateTime != nil {
		return *e.Provenance.SourceUpdateTime
	}
	return time.Time{}
}
