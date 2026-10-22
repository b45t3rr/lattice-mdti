// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package adsb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Source is any producer of ADS-B frames (HTTP aggregator today, SBS/Beast TCP later).
type Source interface {
	Name() string
	Run(ctx context.Context, out chan<- domain.Frame) error
}

// Provider describes a readsb-compatible aggregator endpoint.
type Provider struct {
	Name        domain.Source
	URLTemplate string        // fmt verbs: lat, lon, radius NM
	MinInterval time.Duration // never poll faster than this
}

// Known providers. adsb.lol enforces 500 requests/day (x-ratelimit header
// "500;w=86400"), so its floor is 180 s; adsb.fi allows 1 request/s.
var Providers = map[string]Provider{
	"adsbfi": {
		Name:        domain.SourceADSBFi,
		URLTemplate: "https://opendata.adsb.fi/api/v3/lat/%.4f/lon/%.4f/dist/%.0f",
		MinInterval: time.Second,
	},
	"adsblol": {
		Name:        domain.SourceADSBLol,
		URLTemplate: "https://api.adsb.lol/v2/point/%.4f/%.4f/%.0f",
		MinInterval: 180 * time.Second,
	},
}

// HTTPSource polls a provider and emits only new position reports.
type HTTPSource struct {
	Provider  Provider
	CenterLat float64
	CenterLon float64
	RadiusNM  float64
	Interval  time.Duration
	Client    *http.Client
	Log       *slog.Logger
	UserAgent string

	lastSeen map[string]time.Time // entity key → last emitted ObservedAt
	Dropped  atomic.Uint64        // frames dropped because the bus was full
	Polls    atomic.Uint64
}

// NewHTTPSource builds a source for a named provider, clamping the interval
// to the provider's floor.
func NewHTTPSource(provider string, lat, lon, radiusNM float64, interval time.Duration, log *slog.Logger) (*HTTPSource, error) {
	p, ok := Providers[provider]
	if !ok {
		return nil, fmt.Errorf("unknown ADS-B provider %q", provider)
	}
	if interval < p.MinInterval {
		log.Warn("ADS-B poll interval below provider floor, clamping", "provider", provider, "requested", interval, "floor", p.MinInterval)
		interval = p.MinInterval
	}
	return &HTTPSource{
		Provider: p, CenterLat: lat, CenterLon: lon, RadiusNM: radiusNM, Interval: interval,
		Client:    &http.Client{Timeout: 10 * time.Second},
		Log:       log.With("module", "rx", "source", string(p.Name)),
		UserAgent: "lattice-mdti/0.1 (+https://github.com/b45t3rr/lattice-mdti)",
	}, nil
}

func (s *HTTPSource) Name() string { return string(s.Provider.Name) }

func (s *HTTPSource) url() string {
	return fmt.Sprintf(s.Provider.URLTemplate, s.CenterLat, s.CenterLon, s.RadiusNM)
}

// Run polls until ctx is cancelled. Transient errors back off exponentially
// (2 s → 60 s, full jitter) and honour Retry-After.
func (s *HTTPSource) Run(ctx context.Context, out chan<- domain.Frame) error {
	s.lastSeen = map[string]time.Time{}
	backoff := time.Duration(0)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		wait, err := s.poll(ctx, out)
		switch {
		case err == nil:
			backoff = 0
			timer.Reset(s.Interval)
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			backoff = min(max(2*backoff, 2*time.Second), 60*time.Second)
			d := max(time.Duration(rand.Int64N(int64(backoff))), wait)
			s.Log.Warn("ADS-B poll failed", "err", err, "retry_in", d.Round(time.Millisecond))
			timer.Reset(d)
		}
	}
}

var errRetryable = errors.New("retryable status")

func (s *HTTPSource) poll(ctx context.Context, out chan<- domain.Frame) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", s.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	s.Polls.Add(1)
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return retryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("%w: HTTP %d", errRetryable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, err
	}
	now := time.Now()
	frames, err := Parse(body, s.Provider.Name, now)
	if err != nil {
		return 0, err
	}
	emitted := 0
	for _, f := range frames {
		if last, ok := s.lastSeen[f.EntityKey]; ok && !f.ObservedAt.After(last.Add(100*time.Millisecond)) {
			continue // same position report as the previous snapshot
		}
		if err := f.Validate(now); err != nil {
			continue
		}
		s.lastSeen[f.EntityKey] = f.ObservedAt
		select {
		case out <- f:
			emitted++
		default:
			s.Dropped.Add(1)
		}
	}
	// Forget aircraft not reported for 10 minutes so the map stays bounded.
	for k, t := range s.lastSeen {
		if now.Sub(t) > 10*time.Minute {
			delete(s.lastSeen, k)
		}
	}
	s.Log.Debug("ADS-B poll", "aircraft", len(frames), "emitted", emitted)
	return 0, nil
}

func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
		return min(time.Duration(secs)*time.Second, 5*time.Minute)
	}
	if t, err := http.ParseTime(h); err == nil {
		return min(max(time.Until(t), 0), 5*time.Minute)
	}
	return 0
}
