// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package ais

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	goais "github.com/BertoldVdb/go-ais"
	"github.com/BertoldVdb/go-ais/aisnmea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Sentences are produced by the go-ais encoder from synthetic values, so the
// tests carry no third-party AIS data.
func encode(t *testing.T, p goais.Packet) []string {
	t.Helper()
	nc := aisnmea.NMEACodecNew(goais.CodecNew(false, false))
	s := nc.EncodeSentence(aisnmea.VdmPacket{Channel: 1, TalkerID: "AI", MessageType: "VDM", Packet: p})
	require.NotEmpty(t, s)
	return s
}

func newTestNMEA(t *testing.T) *NMEA {
	t.Helper()
	n, err := NewNMEA("udp", "127.0.0.1:0", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	return n
}

func feed(n *NMEA, lines []string, now time.Time) []domain.Frame {
	out := make(chan domain.Frame, 16)
	for _, l := range lines {
		n.HandleLine(l, now, out)
	}
	close(out)
	var fs []domain.Frame
	for f := range out {
		fs = append(fs, f)
	}
	return fs
}

func posReport(mmsi uint32) goais.PositionReport {
	return goais.PositionReport{Header: goais.Header{MessageID: 1, UserID: mmsi}, Valid: true,
		NavigationalStatus: 7, Sog: 9.5, Longitude: -60.8, Latitude: -46.37, Cog: 305, TrueHeading: 304}
}

func TestNMEAPositionWithStaticJoined(t *testing.T) {
	n := newTestNMEA(t)
	now := time.Now()
	static := goais.ShipStaticData{Header: goais.Header{MessageID: 5, UserID: 701999001}, Valid: true,
		ImoNumber: 9999999, CallSign: "LW1234", Name: "SYN TRAWLER", Type: 30,
		Dimension: goais.FieldDimension{A: 30, B: 12, C: 5, D: 5}}
	lines := encode(t, static)
	require.Len(t, lines, 2, "type 5 is a two-sentence message")

	fs := feed(n, append(lines, encode(t, posReport(701999001))...), now)
	require.Len(t, fs, 1)
	f := fs[0]
	assert.Equal(t, "ais:701999001", f.EntityKey)
	assert.Equal(t, domain.SourceNMEA, f.Source)
	assert.Equal(t, "A", f.AISClass)
	assert.InDelta(t, -46.37, f.Lat, 1e-4)
	assert.InDelta(t, -60.8, f.Lon, 1e-4)
	assert.InDelta(t, 9.5*domain.KnotToMps, *f.GroundSpeedMps, 1e-6)
	assert.InDelta(t, 305, *f.TrackDeg, 1e-6)
	assert.Equal(t, 7, *f.NavStatus)
	assert.Equal(t, "SYN TRAWLER", f.ShipName)
	assert.Equal(t, "9999999", f.IMO)
	assert.Equal(t, 30, *f.ShipType)
	assert.InDelta(t, 42, *f.LengthM, 1e-9)
}

func TestNMEAClassBAndUnavailableFields(t *testing.T) {
	n := newTestNMEA(t)
	b := goais.StandardClassBPositionReport{Header: goais.Header{MessageID: 18, UserID: 701999002}, Valid: true,
		Sog: 102.3, Longitude: -58.2, Latitude: -34.7, Cog: 360, TrueHeading: 511}
	fs := feed(n, encode(t, b), time.Now())
	require.Len(t, fs, 1)
	assert.Equal(t, "B", fs[0].AISClass)
	assert.Nil(t, fs[0].GroundSpeedMps, "102.3 kn means not available")
	assert.Nil(t, fs[0].TrackDeg, "360 means not available")
	assert.Nil(t, fs[0].HeadingDeg, "511 means not available")

	none := goais.StandardClassBPositionReport{Header: goais.Header{MessageID: 18, UserID: 701999003}, Valid: true,
		Longitude: 181, Latitude: 91}
	assert.Empty(t, feed(n, encode(t, none), time.Now()), "no position, no frame")
}

func TestNMEARejectsBadInput(t *testing.T) {
	n := newTestNMEA(t)
	good := encode(t, posReport(701999004))[0]
	bad := good[:len(good)-2] + "00" // wrong checksum
	fs := feed(n, []string{bad, "$GPGGA,not,ais", "", "garbage"}, time.Now())
	assert.Empty(t, fs)
	assert.Equal(t, uint64(1), n.Invalid.Load(), "the corrupted sentence is counted")
	// A tag-block prefix ("\c:...\!AIVDM...") is tolerated.
	assert.Len(t, feed(n, []string{`\c:1700000000*5A\` + good}, time.Now()), 1)
}

func TestNMEAOverUDP(t *testing.T) {
	n := newTestNMEA(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Pick a free port, then listen on it.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	n.Addr = pc.LocalAddr().String()
	pc.Close()
	out := make(chan domain.Frame, 4)
	go func() { _ = n.Run(ctx, out) }()

	conn, err := net.Dial("udp", n.Addr)
	require.NoError(t, err)
	defer conn.Close()
	payload := strings.Join(encode(t, posReport(701999005)), "\r\n") + "\r\n"
	var f domain.Frame
	require.Eventually(t, func() bool {
		_, _ = conn.Write([]byte(payload))
		select {
		case f = <-out:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}, 3*time.Second, 10*time.Millisecond)
	assert.Equal(t, "ais:701999005", f.EntityKey)
}

func TestNewNMEAValidates(t *testing.T) {
	_, err := NewNMEA("serial", ":10110", nil)
	assert.Error(t, err)
	_, err = NewNMEA("udp", "10110", nil)
	assert.Error(t, err)
}
