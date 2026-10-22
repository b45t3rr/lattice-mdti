// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package ais

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

func posMsg(mmsi int64, lat, lon, sog, cog float64, heading int, ts time.Time) string {
	return fmt.Sprintf(`{"MessageType":"PositionReport","MetaData":{"MMSI":%d,"ShipName":"X","latitude":%f,"longitude":%f,"time_utc":"%s"},
		"Message":{"PositionReport":{"UserID":%d,"Valid":true,"NavigationalStatus":7,"Sog":%f,"Cog":%f,"TrueHeading":%d,"Latitude":%f,"Longitude":%f}}}`,
		mmsi, lat, lon, ts.UTC().Format("2006-01-02 15:04:05.000000000 -0700 MST"), mmsi, sog, cog, heading, lat, lon)
}

const staticMsg = `{"MessageType":"ShipStaticData","MetaData":{"MMSI":412000001},
	"Message":{"ShipStaticData":{"UserID":412000001,"Valid":true,"ImoNumber":9123456,"CallSign":"BZAB1 ","Name":"LU YUAN YU 001     ","Type":30,
	"Dimension":{"A":40,"B":15,"C":5,"D":4}}}}`

func TestDecodePositionReport(t *testing.T) {
	ts := time.Date(2026, 10, 21, 19, 0, 0, 123_000_000, time.UTC)
	d, err := Decode([]byte(posMsg(701000001, -46.0, -60.55, 9.0, 270, 268, ts)), time.Now())
	require.NoError(t, err)
	require.NotNil(t, d)
	require.NotNil(t, d.Frame)
	f := d.Frame
	assert.Equal(t, "ais:701000001", f.EntityKey)
	assert.Equal(t, domain.DomainSea, f.Domain)
	assert.Equal(t, "A", f.AISClass)
	assert.True(t, f.ObservedAt.Equal(ts), "time_utc parsed: %v", f.ObservedAt)
	assert.InDelta(t, 9.0*domain.KnotToMps, *f.GroundSpeedMps, 1e-9)
	assert.Equal(t, 270.0, *f.TrackDeg)
	assert.Equal(t, 268.0, *f.HeadingDeg)
	assert.Equal(t, 7, *f.NavStatus)
}

func TestDecodeUnavailableSentinels(t *testing.T) {
	d, err := Decode([]byte(posMsg(701000002, -38, -57, 102.3, 360, 511, time.Now())), time.Now())
	require.NoError(t, err)
	assert.Nil(t, d.Frame.GroundSpeedMps)
	assert.Nil(t, d.Frame.TrackDeg)
	assert.Nil(t, d.Frame.HeadingDeg)

	d, err = Decode([]byte(posMsg(701000003, 91, 181, 1, 1, 1, time.Now())), time.Now())
	require.NoError(t, err)
	assert.Nil(t, d, "unavailable position dropped")
}

func TestDecodeStaticAndClassB(t *testing.T) {
	d, err := Decode([]byte(staticMsg), time.Now())
	require.NoError(t, err)
	require.NotNil(t, d.Static)
	assert.Nil(t, d.Frame)
	assert.Equal(t, "LU YUAN YU 001", d.Static.Name)
	assert.Equal(t, "BZAB1", d.Static.CallSign)
	assert.Equal(t, "9123456", d.Static.IMO)
	assert.Equal(t, 30, *d.Static.ShipType)
	assert.Equal(t, 55.0, *d.Static.LengthM)
	assert.Equal(t, 9.0, *d.Static.BeamM)

	b := `{"MessageType":"StandardClassBPositionReport","MetaData":{},"Message":{"StandardClassBPositionReport":
		{"UserID":701999999,"Valid":true,"Sog":5,"Cog":10,"TrueHeading":511,"Latitude":-38.1,"Longitude":-57.5}}}`
	d, err = Decode([]byte(b), time.Now())
	require.NoError(t, err)
	assert.Equal(t, "B", d.Frame.AISClass)
	assert.Nil(t, d.Frame.NavStatus)
}

func TestDecodeIgnoresUnknownType(t *testing.T) {
	d, err := Decode([]byte(`{"MessageType":"BaseStationReport","MetaData":{},"Message":{"BaseStationReport":{}}}`), time.Now())
	require.NoError(t, err)
	assert.Nil(t, d)
	_, err = Decode([]byte(`{not json`), time.Now())
	assert.Error(t, err)
}

func TestStreamSubscribesJoinsStaticAndReconnects(t *testing.T) {
	subs := make(chan subscription, 4)
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		conns++
		ctx := r.Context()
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var s subscription
		_ = json.Unmarshal(data, &s)
		subs <- s
		if conns == 1 {
			_ = c.Write(ctx, websocket.MessageText, []byte(staticMsg))
			_ = c.Write(ctx, websocket.MessageText, []byte(posMsg(412000001, -46.0, -60.55, 9, 270, 270, time.Now())))
			c.Close(websocket.StatusGoingAway, "bye") // force a reconnect
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(posMsg(412000001, -46.0, -60.60, 9, 270, 270, time.Now())))
		<-ctx.Done()
	}))
	defer srv.Close()

	s := NewStream("test-key", [4]float64{-56, -70, -36, -52}, 5*time.Second, slog.New(slog.DiscardHandler))
	s.URL = "ws" + strings.TrimPrefix(srv.URL, "http")
	out := make(chan domain.Frame, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = s.Run(ctx, out) }()

	first := <-out
	assert.Equal(t, "LU YUAN YU 001", first.ShipName, "static joined onto position")
	assert.Equal(t, 30, *first.ShipType)
	second := <-out
	assert.InDelta(t, -60.60, second.Lon, 1e-9, "received after reconnect")
	assert.Equal(t, "LU YUAN YU 001", second.ShipName, "static cache survives reconnect")

	sub := <-subs
	assert.Equal(t, "test-key", sub.APIKey)
	assert.Equal(t, [][][2]float64{{{-56, -70}, {-36, -52}}}, sub.BoundingBoxes)
	assert.ElementsMatch(t, SubscribedTypes, sub.FilterMessageTypes)
}

func TestStreamRequiresAPIKey(t *testing.T) {
	s := NewStream("", [4]float64{}, time.Second, slog.New(slog.DiscardHandler))
	assert.Error(t, s.Run(context.Background(), make(chan domain.Frame)))
}
