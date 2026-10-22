// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package ais

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/b45t3rr/lattice-mdti/internal/config"
	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// DefaultURL is the AISStream WebSocket endpoint.
const DefaultURL = "wss://stream.aisstream.io/v0/stream"

// Stream is a reconnecting AISStream client. AISStream drops messages for slow
// readers and never replays, so the read loop never blocks on consumers: a
// full output channel drops the frame and counts it.
type Stream struct {
	URL         string
	APIKey      config.Secret
	BBox        [4]float64 // minLat, minLon, maxLat, maxLon
	IdleTimeout time.Duration
	Log         *slog.Logger

	static *staticCache

	Received atomic.Uint64
	Dropped  atomic.Uint64
	Invalid  atomic.Uint64
}

type subscription struct {
	APIKey             string         `json:"APIKey"`
	BoundingBoxes      [][][2]float64 `json:"BoundingBoxes"`
	FilterMessageTypes []string       `json:"FilterMessageTypes"`
}

func NewStream(apiKey config.Secret, bbox [4]float64, idle time.Duration, log *slog.Logger) *Stream {
	return &Stream{
		URL: DefaultURL, APIKey: apiKey, BBox: bbox, IdleTimeout: idle,
		Log: log.With("module", "rx", "source", string(domain.SourceAISStream)),
	}
}

func (s *Stream) Name() string { return string(domain.SourceAISStream) }

// Run connects, subscribes and reads until ctx is cancelled, reconnecting with
// exponential backoff (1 s → 60 s, full jitter).
func (s *Stream) Run(ctx context.Context, out chan<- domain.Frame) error {
	if s.APIKey.Reveal() == "" {
		return errors.New("AISSTREAM_API_KEY is not set")
	}
	if s.static == nil {
		s.static = newStaticCache(staticMax, staticTTL)
	}
	backoff := time.Duration(0)
	for {
		start := time.Now()
		err := s.session(ctx, out)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			backoff = 0 // the session was healthy; start over
		}
		backoff = min(max(2*backoff, time.Second), 60*time.Second)
		d := time.Duration(rand.Int64N(int64(backoff)) + 1)
		s.Log.Warn("AISStream disconnected", "err", err, "retry_in", d.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}

func (s *Stream) session(ctx context.Context, out chan<- domain.Frame) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, s.URL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	sub, err := json.Marshal(subscription{
		APIKey:             s.APIKey.Reveal(),
		BoundingBoxes:      [][][2]float64{{{s.BBox[0], s.BBox[1]}, {s.BBox[2], s.BBox[3]}}},
		FilterMessageTypes: SubscribedTypes,
	})
	if err != nil {
		return err
	}
	// AISStream closes the connection if the subscription is not sent within 3 s.
	wctx, wcancel := context.WithTimeout(ctx, 3*time.Second)
	err = conn.Write(wctx, websocket.MessageText, sub)
	wcancel()
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	s.Log.Info("AISStream subscribed", "bbox", s.BBox)

	for {
		rctx, rcancel := context.WithTimeout(ctx, s.IdleTimeout)
		_, data, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		s.handle(data, out)
	}
}

func (s *Stream) handle(data []byte, out chan<- domain.Frame) {
	now := time.Now()
	s.Received.Add(1)
	d, err := Decode(data, now)
	if err != nil {
		s.Invalid.Add(1)
		s.Log.Debug("AIS decode failed", "err", err)
		return
	}
	if d == nil {
		return
	}
	if d.Static != nil {
		s.static.merge(d.MMSI, *d.Static, now)
	}
	if d.Frame == nil {
		return
	}
	f := *d.Frame
	if st, ok := s.static.get(d.MMSI, now); ok {
		ApplyStatic(&f, st)
	}
	if err := f.Validate(now); err != nil {
		s.Invalid.Add(1)
		return
	}
	select {
	case out <- f:
	default:
		s.Dropped.Add(1)
	}
}

// staticCache is a bounded TTL map of per-MMSI static data. It is owned by the
// single read goroutine, so it needs no locking.
type staticCache struct {
	max int
	ttl time.Duration
	m   map[string]staticEntry
}

type staticEntry struct {
	s  Static
	at time.Time
}

func newStaticCache(max int, ttl time.Duration) *staticCache {
	return &staticCache{max: max, ttl: ttl, m: map[string]staticEntry{}}
}

func (c *staticCache) merge(mmsi string, s Static, now time.Time) {
	e, ok := c.m[mmsi]
	if !ok && len(c.m) >= c.max {
		c.evict(now)
	}
	e.s.merge(s)
	e.at = now
	c.m[mmsi] = e
}

func (c *staticCache) get(mmsi string, now time.Time) (Static, bool) {
	e, ok := c.m[mmsi]
	if !ok || now.Sub(e.at) > c.ttl {
		return Static{}, false
	}
	return e.s, true
}

// evict drops expired entries, or the oldest tenth if none expired.
func (c *staticCache) evict(now time.Time) {
	for k, e := range c.m {
		if now.Sub(e.at) > c.ttl {
			delete(c.m, k)
		}
	}
	if len(c.m) < c.max {
		return
	}
	cutoff := now
	n := 0
	for _, e := range c.m {
		if e.at.Before(cutoff) {
			cutoff = e.at
		}
	}
	for k, e := range c.m {
		if n >= c.max/10 {
			break
		}
		if e.at.Sub(cutoff) < c.ttl/10 {
			delete(c.m, k)
			n++
		}
	}
}
