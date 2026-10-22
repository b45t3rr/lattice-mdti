// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package sbs reads ADS-B traffic from a local receiver in BaseStation (SBS-1)
// format, the text feed dump1090, readsb and Virtual Radar Server publish on
// TCP port 30003.
//
// SBS lines carry partial updates (identification, position, velocity,
// altitude, squawk), so the adapter keeps the latest value of each field per
// aircraft and emits a frame on every position update. SBS has no geometric
// altitude and no navigation-integrity fields: those frame fields stay nil
// (not reported), so the baro/GNSS and integrity detectors simply do not run
// on SBS tracks. Use the receiver's aircraft.json source when they matter.
package sbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Source connects to an SBS feed as a TCP client and reconnects with backoff.
type Source struct {
	Addr string // "host:30003"
	Log  *slog.Logger

	Received, Invalid, Dropped atomic.Uint64

	state    map[string]*aircraft
	lastSeen time.Time
}

type aircraft struct {
	callsign, squawk string
	altFt, gsKt      *float64
	trackDeg, vrate  *float64
	onGround         *bool
	seen             time.Time
}

// stale is how long an aircraft's state is kept without updates.
const stale = 5 * time.Minute

// New validates the address.
func New(addr string, log *slog.Logger) (*Source, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("sbs: address %q: %w", addr, err)
	}
	return &Source{Addr: addr, Log: log, state: map[string]*aircraft{}}, nil
}

// Name implements the source interface.
func (s *Source) Name() string { return string(domain.SourceSBS) }

// Run reads the feed until ctx ends.
func (s *Source) Run(ctx context.Context, out chan<- domain.Frame) error {
	backoff := time.Second
	for {
		err := s.session(ctx, out)
		if ctx.Err() != nil {
			return nil
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		s.Log.Warn("SBS connection lost, reconnecting", "addr", s.Addr, "err", err, "in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (s *Source) session(ctx context.Context, out chan<- domain.Frame) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()
	s.Log.Info("SBS connected", "addr", s.Addr)
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		s.HandleLine(sc.Text(), time.Now(), out)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("connection closed by peer")
}

// HandleLine applies one SBS line and emits a frame when it carries a
// position. It must be called from a single goroutine.
//
// Fields (0-based): 0 MSG, 1 transmission type, 4 ICAO hex, 10 callsign,
// 11 altitude (ft, baro), 12 ground speed (kt), 13 track, 14 lat, 15 lon,
// 16 vertical rate (ft/min), 17 squawk, 21 on ground (-1 = true).
func (s *Source) HandleLine(line string, now time.Time, out chan<- domain.Frame) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	s.Received.Add(1)
	f := strings.Split(line, ",")
	if len(f) < 22 || f[0] != "MSG" {
		s.Invalid.Add(1)
		return
	}
	hex := strings.ToLower(strings.TrimSpace(f[4]))
	if !validHex(hex) {
		s.Invalid.Add(1)
		return
	}
	if now.Sub(s.lastSeen) > time.Minute {
		s.evict(now)
		s.lastSeen = now
	}
	a := s.state[hex]
	if a == nil {
		a = &aircraft{}
		s.state[hex] = a
	}
	a.seen = now
	if cs := domain.SanitizeIdent(f[10]); cs != "" {
		a.callsign = cs
	}
	setFloat(&a.altFt, f[11])
	setFloat(&a.gsKt, f[12])
	setFloat(&a.trackDeg, f[13])
	setFloat(&a.vrate, f[16])
	if sq := strings.TrimSpace(f[17]); validSquawk(sq) {
		a.squawk = sq
	}
	switch strings.TrimSpace(f[21]) {
	case "-1", "1":
		a.onGround = domain.Ptr(true)
	case "0":
		a.onGround = domain.Ptr(false)
	}

	lat, errLat := strconv.ParseFloat(strings.TrimSpace(f[14]), 64)
	lon, errLon := strconv.ParseFloat(strings.TrimSpace(f[15]), 64)
	if errLat != nil || errLon != nil {
		return // not a position update
	}
	fr := domain.Frame{
		EntityKey: domain.ADSBKey(hex), Domain: domain.DomainAir, Source: domain.SourceSBS,
		ObservedAt: now, ReceivedAt: now, Lat: lat, Lon: lon,
		AltBaroFt: a.altFt, TrackDeg: a.trackDeg, VertRateBaroFpm: a.vrate, OnGround: a.onGround,
		Callsign: a.callsign, Squawk: a.squawk,
	}
	if a.gsKt != nil {
		fr.GroundSpeedMps = domain.Ptr(*a.gsKt * domain.KnotToMps)
	}
	if err := fr.Validate(now); err != nil {
		s.Invalid.Add(1)
		return
	}
	select {
	case out <- fr:
	default:
		s.Dropped.Add(1)
	}
}

func (s *Source) evict(now time.Time) {
	for k, a := range s.state {
		if now.Sub(a.seen) > stale {
			delete(s.state, k)
		}
	}
}

// setFloat updates *dst when the field is present; empty fields keep the
// last known value (SBS lines are partial updates).
func setFloat(dst **float64, field string) {
	field = strings.TrimSpace(field)
	if field == "" {
		return
	}
	if v, err := strconv.ParseFloat(field, 64); err == nil {
		*dst = domain.Ptr(v)
	}
}

func validHex(h string) bool {
	if len(h) != 6 {
		return false
	}
	_, err := strconv.ParseUint(h, 16, 32)
	return err == nil
}

func validSquawk(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '7' {
			return false
		}
	}
	return true
}
