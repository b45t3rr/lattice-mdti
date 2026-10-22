// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package ais

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

	goais "github.com/BertoldVdb/go-ais"
	"github.com/BertoldVdb/go-ais/aisnmea"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// NMEA reads raw AIS sentences (!AIVDM / !AIVDO, one per line) from a local
// receiver: AIS-catcher, rtl-ais, a dAISy or any feed that forwards NMEA 0183.
// Over UDP it listens on Addr; over TCP it connects to Addr as a client and
// reconnects with backoff. Multi-fragment messages are reassembled.
type NMEA struct {
	Network string // "udp" or "tcp"
	Addr    string // UDP: listen address (":10110"); TCP: server address ("host:10110")
	Log     *slog.Logger

	Received, Invalid, Dropped atomic.Uint64

	static *staticCache
	codec  *aisnmea.NMEACodec
}

// NewNMEA returns a source for network "udp" or "tcp".
func NewNMEA(network, addr string, log *slog.Logger) (*NMEA, error) {
	if network != "udp" && network != "tcp" {
		return nil, fmt.Errorf("nmea: network must be udp or tcp, got %q", network)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("nmea: address %q: %w", addr, err)
	}
	return &NMEA{Network: network, Addr: addr, Log: log,
		static: newStaticCache(staticMax, staticTTL),
		codec:  aisnmea.NMEACodecNew(goais.CodecNew(false, false))}, nil
}

// Static data limits, as for the AISStream source.
const (
	staticMax = 50_000
	staticTTL = 24 * time.Hour
)

// Name implements the source interface.
func (n *NMEA) Name() string { return string(domain.SourceNMEA) }

// Run reads sentences until ctx ends.
func (n *NMEA) Run(ctx context.Context, out chan<- domain.Frame) error {
	if n.Network == "udp" {
		return n.runUDP(ctx, out)
	}
	backoff := time.Second
	for {
		err := n.runTCP(ctx, out)
		if ctx.Err() != nil {
			return nil
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		n.Log.Warn("AIS NMEA connection lost, reconnecting", "addr", n.Addr, "err", err, "in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (n *NMEA) runUDP(ctx context.Context, out chan<- domain.Frame) error {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(ctx, "udp", n.Addr)
	if err != nil {
		return fmt.Errorf("nmea: listen %s: %w", n.Addr, err)
	}
	defer pc.Close()
	go func() { <-ctx.Done(); pc.Close() }()
	n.Log.Info("AIS NMEA listening", "udp", pc.LocalAddr().String())
	buf := make([]byte, 64<<10)
	for {
		k, _, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, line := range strings.Split(string(buf[:k]), "\n") {
			n.HandleLine(line, time.Now(), out)
		}
	}
}

func (n *NMEA) runTCP(ctx context.Context, out chan<- domain.Frame) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", n.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()
	n.Log.Info("AIS NMEA connected", "tcp", n.Addr)
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		n.HandleLine(sc.Text(), time.Now(), out)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("connection closed by peer")
}

// HandleLine decodes one sentence and emits a frame when it completes a
// position report. It is safe to call from one goroutine only.
func (n *NMEA) HandleLine(line string, now time.Time, out chan<- domain.Frame) {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, "!$"); i > 0 {
		line = line[i:] // drop a leading tag block or timestamp prefix
	}
	if line == "" || line[0] != '!' {
		return
	}
	n.Received.Add(1)
	pkt, err := n.codec.ParseSentence(line)
	if err != nil {
		n.Invalid.Add(1)
		n.Log.Debug("AIS NMEA parse failed", "err", err)
		return
	}
	if pkt == nil || pkt.Packet == nil {
		return // first fragment of a multi-sentence message, or an undecodable payload
	}
	d := DecodePacket(pkt.Packet, now)
	if d == nil {
		return
	}
	if d.Static != nil {
		n.static.merge(d.MMSI, *d.Static, now)
	}
	if d.Frame == nil {
		return
	}
	f := *d.Frame
	if st, ok := n.static.get(d.MMSI, now); ok {
		ApplyStatic(&f, st)
	}
	if err := f.Validate(now); err != nil {
		n.Invalid.Add(1)
		return
	}
	select {
	case out <- f:
	default:
		n.Dropped.Add(1)
	}
}

// DecodePacket converts a decoded AIS message into a frame and/or a static
// update, with the same rules as the AISStream decoder. Unsupported message
// types return nil.
func DecodePacket(p goais.Packet, receivedAt time.Time) *Decoded {
	pos := func(id uint32, lat, lon, sog, cog float64, heading uint16, class string) *Decoded {
		if id == 0 || lat == latUnavailable || lon == lonUnavailable {
			return nil
		}
		mmsi := strconv.FormatUint(uint64(id), 10)
		f := domain.Frame{
			EntityKey: domain.AISKey(mmsi), Domain: domain.DomainSea, Source: domain.SourceNMEA,
			ObservedAt: receivedAt, ReceivedAt: receivedAt, Lat: lat, Lon: lon, MMSI: mmsi, AISClass: class,
		}
		if sog < sogUnavailable {
			f.GroundSpeedMps = domain.Ptr(sog * domain.KnotToMps)
		}
		if cog < cogUnavailable {
			f.TrackDeg = domain.Ptr(cog)
		}
		if heading != headingUnavailable && heading < 360 {
			f.HeadingDeg = domain.Ptr(float64(heading))
		}
		return &Decoded{MMSI: mmsi, Frame: &f}
	}
	shipType := func(t uint8) *int {
		if t == 0 {
			return nil
		}
		return domain.Ptr(int(t))
	}
	switch m := p.(type) {
	case goais.PositionReport:
		if !m.Valid {
			return nil
		}
		d := pos(m.UserID, float64(m.Latitude), float64(m.Longitude), float64(m.Sog), float64(m.Cog), m.TrueHeading, "A")
		if d != nil {
			d.Frame.NavStatus = domain.Ptr(int(m.NavigationalStatus))
		}
		return d
	case goais.StandardClassBPositionReport:
		if !m.Valid {
			return nil
		}
		return pos(m.UserID, float64(m.Latitude), float64(m.Longitude), float64(m.Sog), float64(m.Cog), m.TrueHeading, "B")
	case goais.ExtendedClassBPositionReport:
		if !m.Valid {
			return nil
		}
		d := pos(m.UserID, float64(m.Latitude), float64(m.Longitude), float64(m.Sog), float64(m.Cog), m.TrueHeading, "B")
		if d != nil {
			st := Static{Name: domain.SanitizeIdent(m.Name), ShipType: shipType(m.Type)}
			st.LengthM, st.BeamM = dimsOf(m.Dimension)
			d.Static = &st
		}
		return d
	case goais.ShipStaticData:
		if !m.Valid || m.UserID == 0 {
			return nil
		}
		st := Static{Name: domain.SanitizeIdent(m.Name), CallSign: domain.SanitizeIdent(m.CallSign), ShipType: shipType(m.Type)}
		if m.ImoNumber > 0 {
			st.IMO = strconv.FormatUint(uint64(m.ImoNumber), 10)
		}
		st.LengthM, st.BeamM = dimsOf(m.Dimension)
		return &Decoded{MMSI: strconv.FormatUint(uint64(m.UserID), 10), Static: &st}
	case goais.StaticDataReport:
		if !m.Valid || m.UserID == 0 {
			return nil
		}
		var st Static
		if m.ReportA.Valid {
			st.Name = domain.SanitizeIdent(m.ReportA.Name)
		}
		if m.ReportB.Valid {
			st.CallSign = domain.SanitizeIdent(m.ReportB.CallSign)
			st.ShipType = shipType(m.ReportB.ShipType)
			st.LengthM, st.BeamM = dimsOf(m.ReportB.Dimension)
		}
		return &Decoded{MMSI: strconv.FormatUint(uint64(m.UserID), 10), Static: &st}
	}
	return nil
}

func dimsOf(d goais.FieldDimension) (length, beam *float64) {
	return dims(&dimension{A: int(d.A), B: int(d.B), C: int(d.C), D: int(d.D)})
}
