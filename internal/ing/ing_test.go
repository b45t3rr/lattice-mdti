// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package ing

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/core"
	"github.com/anduril/lattice-sdk-go/v5/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/rx/adsb"
)

var update = flag.Bool("update", false, "rewrite golden files")

var t0 = time.Date(2026, 10, 21, 19, 41, 42, 0, time.UTC)

func mapper() *Mapper { return &Mapper{IntegrationName: "mdti", Now: func() time.Time { return t0 }} }

func liveADSBFrame(t *testing.T) domain.Frame {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/adsb/synthetic_readsb_v3.json")
	require.NoError(t, err)
	frames, err := adsb.Parse(raw, domain.SourceADSBFi, t0)
	require.NoError(t, err)
	for _, f := range frames {
		if f.EntityKey == "adsb:e0aa11" {
			f.ObservedAt = t0
			return f
		}
	}
	t.Fatal("fixture aircraft missing")
	return domain.Frame{}
}

func aisFrame() domain.Frame {
	return domain.Frame{
		EntityKey: "ais:701000677", Domain: domain.DomainSea, Source: domain.SourceAISStream, ObservedAt: t0,
		Lat: -38.0463, Lon: -57.5312, GroundSpeedMps: domain.Ptr(4.0), TrackDeg: domain.Ptr(290.0),
		MMSI: "701000677", IMO: "9123456", ShipName: "PESQUERO UNO", ShipCall: "LW1234", ShipType: domain.Ptr(30),
		AISClass: "A", LengthM: domain.Ptr(42.0),
	}
}

func golden(t *testing.T, name string, e *Lattice.Entity) {
	t.Helper()
	got, err := json.MarshalIndent(e, "", "  ")
	require.NoError(t, err)
	path := "../../testdata/golden/" + name
	if *update {
		require.NoError(t, os.MkdirAll("../../testdata/golden", 0o755))
		require.NoError(t, os.WriteFile(path, append(got, '\n'), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run `go test ./internal/ing -update` to create golden files")
	assert.JSONEq(t, string(want), string(got))
}

func TestMapADSBGolden(t *testing.T) {
	f := liveADSBFrame(t)
	e := mapper().Entity(f, t0.Add(-time.Minute))
	golden(t, "entity_adsb.json", e)

	assert.Equal(t, "mdti-adsb-e0aa11", *e.EntityID)
	assert.Equal(t, "TST101", *e.Aliases.Name)
	assert.Equal(t, Lattice.MilViewDispositionDispositionNeutral, *e.MilView.Disposition,
		"cooperative A1–A5 with callsign and squawk")
	assert.Equal(t, "SNAPCF---------", *e.Symbology.MilStd2525C.Sidc)
	assert.Equal(t, 0xe0aa11, *e.TransponderCodes.ModeS.Address)
	assert.Equal(t, 1633, *e.TransponderCodes.Mode3)
	assert.Equal(t, t0.Add(ADSBExpiry), *e.ExpiryTime)
	assert.Equal(t, 4, *e.Tracked.TrackQualityWrapper, "NACp 9")
	assert.InDelta(t, *f.AltGeomFt*domain.FtToM, *e.Location.Position.AltitudeHaeMeters, 1e-9, "geometric altitude is HAE")
}

func TestMapAISGolden(t *testing.T) {
	e := mapper().Entity(aisFrame(), t0)
	golden(t, "entity_ais.json", e)

	assert.Equal(t, "mdti-ais-701000677", *e.EntityID)
	assert.Equal(t, Lattice.MilViewNationality("NATIONALITY_ARGENTINA"), *e.MilView.Nationality)
	assert.Equal(t, Lattice.MilViewDispositionDispositionNeutral, *e.MilView.Disposition)
	assert.Equal(t, "SNSPXF---------", *e.Symbology.MilStd2525C.Sidc)
	assert.Equal(t, "Fishing Vessel", *e.Ontology.PlatformType)
	assert.Equal(t, 42.0, *e.Dimensions.LengthM)
	assert.Equal(t, t0.Add(AISExpiry), *e.ExpiryTime)
	assert.Nil(t, e.TransponderCodes)
	assert.Len(t, e.Aliases.AlternateIDs, 3)
}

// Every enum literal the mapper emits must parse with the SDK's own parser;
// the SDK types accept any string locally and only the server rejects them.
func TestMappedLiteralsAreValid(t *testing.T) {
	for _, e := range []*Lattice.Entity{mapper().Entity(liveADSBFrame(t), t0), mapper().Entity(aisFrame(), t0)} {
		_, err := Lattice.NewMilViewDispositionFromString(string(*e.MilView.Disposition))
		assert.NoError(t, err)
		_, err = Lattice.NewMilViewEnvironmentFromString(string(*e.MilView.Environment))
		assert.NoError(t, err)
		_, err = Lattice.NewOntologyTemplateFromString(string(*e.Ontology.Template))
		assert.NoError(t, err)
		_, err = Lattice.NewClassificationInformationLevelFromString(string(*e.DataClassification.Default.Level))
		assert.NoError(t, err)
		for _, a := range e.Aliases.AlternateIDs {
			_, err = Lattice.NewAlternateIDTypeFromString(string(*a.Type))
			assert.NoError(t, err)
		}
		if e.MilView.Nationality != nil {
			_, err = Lattice.NewMilViewNationalityFromString(string(*e.MilView.Nationality))
			assert.NoError(t, err)
		}
		assert.Len(t, *e.Symbology.MilStd2525C.Sidc, 15)
	}
}

func TestInitialDisposition(t *testing.T) {
	airliner := domain.Frame{Domain: domain.DomainAir, Callsign: "ARG1350", Squawk: "1543", Category: "A3"}
	assert.Equal(t, Lattice.MilViewDispositionDispositionNeutral, InitialDisposition(airliner))
	ghost := airliner
	ghost.Callsign = ""
	assert.Equal(t, Lattice.MilViewDispositionDispositionUnknown, InitialDisposition(ghost))
	mlat := airliner
	mlat.IsMLAT = true
	assert.Equal(t, Lattice.MilViewDispositionDispositionUnknown, InitialDisposition(mlat))
	uav := airliner
	uav.Category = "B6"
	assert.Equal(t, Lattice.MilViewDispositionDispositionUnknown, InitialDisposition(uav))
	vessel := domain.Frame{Domain: domain.DomainSea}
	assert.Equal(t, Lattice.MilViewDispositionDispositionUnknown, InitialDisposition(vessel))
}

func TestExpiryUsesLocalClockWhenSkewed(t *testing.T) {
	f := aisFrame()
	f.ObservedAt = t0.Add(-time.Hour) // replayed recording
	e := mapper().Entity(f, t0)
	assert.Equal(t, t0.Add(AISExpiry), *e.ExpiryTime, "expiry must be in the future")
	assert.Equal(t, f.ObservedAt, *e.Provenance.SourceUpdateTime)
}

func TestYawQuaternion(t *testing.T) {
	q := yawQuaternion(90) // heading east → ENU yaw 0
	assert.InDelta(t, 0, *q.Z, 1e-12)
	assert.InDelta(t, 1, *q.W, 1e-12)
	q = yawQuaternion(0) // heading north → ENU yaw +90°
	assert.InDelta(t, 0.7071, *q.Z, 1e-4)
	assert.InDelta(t, 0.7071, *q.W, 1e-4)
}

// ---------------------------------------------------------------------------
// publisher

type fakeAPI struct {
	mu    sync.Mutex
	calls []*Lattice.Entity
	errFn func(*Lattice.Entity) error
}

func (f *fakeAPI) PublishEntity(_ context.Context, e *Lattice.Entity, _ ...option.RequestOption) (*Lattice.Entity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, e)
	if f.errFn != nil {
		if err := f.errFn(e); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func (f *fakeAPI) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func entityAt(id string, at time.Time) *Lattice.Entity {
	return &Lattice.Entity{EntityID: Lattice.String(id), Provenance: &Lattice.Provenance{SourceUpdateTime: &at}}
}

func newTestPublisher(api EntityAPI) *Publisher {
	p := NewPublisher(api, 1000, slog.New(slog.DiscardHandler))
	return p
}

func TestPublisherCoalescesLatestWins(t *testing.T) {
	api := &fakeAPI{}
	p := newTestPublisher(api)
	for i := range 5 {
		p.Submit(entityAt("mdti-adsb-a", t0.Add(time.Duration(i)*time.Second)))
	}
	p.Submit(entityAt("mdti-adsb-a", t0)) // older than queued: ignored
	p.Submit(entityAt("mdti-adsb-b", t0))
	p.Flush(context.Background())
	require.Equal(t, 2, api.n())
	assert.Equal(t, uint64(4), p.Superseded.Load())
	assert.Equal(t, uint64(1), p.Stale.Load())
	for _, e := range api.calls {
		if *e.EntityID == "mdti-adsb-a" {
			assert.Equal(t, t0.Add(4*time.Second), *e.Provenance.SourceUpdateTime)
		}
	}
	// Already sent: an equal or older update is stale.
	p.Submit(entityAt("mdti-adsb-a", t0.Add(4*time.Second)))
	assert.Equal(t, 0, p.Pending())
}

func TestPublisherRejectsInvalidWithoutRetry(t *testing.T) {
	api := &fakeAPI{errFn: func(*Lattice.Entity) error {
		return core.NewAPIError(400, nil, errors.New("UNMARSHAL_ERROR: invalid value for enum field"))
	}}
	p := newTestPublisher(api)
	p.Submit(entityAt("mdti-adsb-a", t0))
	p.Flush(context.Background())
	p.Flush(context.Background())
	assert.Equal(t, 1, api.n(), "a 400 is never retried")
	assert.Equal(t, uint64(1), p.Rejected.Load())
}

func TestPublisherRetries5xxAndThrottlesOn429(t *testing.T) {
	var fail = map[int]int{} // call index → status
	fail[0], fail[1] = 503, 429
	i := 0
	api := &fakeAPI{errFn: func(*Lattice.Entity) error {
		defer func() { i++ }()
		if code, ok := fail[i]; ok {
			return core.NewAPIError(code, nil, errors.New("x"))
		}
		return nil
	}}
	p := newTestPublisher(api)
	p.Submit(entityAt("mdti-adsb-a", t0))
	p.Flush(context.Background()) // 503 → requeued
	assert.Equal(t, 1, p.Pending())
	p.Flush(context.Background()) // 429 → requeued + throttled
	assert.Equal(t, 1, p.Pending())
	assert.Equal(t, 500.0, float64(p.limiter.Limit()), "rate halved")
	p.Flush(context.Background()) // success
	assert.Equal(t, 0, p.Pending())
	assert.Equal(t, uint64(1), p.Published.Load())
	assert.Equal(t, 500.0, float64(p.limiter.Limit()), "no recovery inside the 30 s backoff window")
}

// ---------------------------------------------------------------------------
// engine

func TestEngineDeadReckoning(t *testing.T) {
	now := t0
	api := &fakeAPI{}
	p := newTestPublisher(api)
	m := mapper()
	m.Now = func() time.Time { return now }
	e := NewEngine(m, p, slog.New(slog.DiscardHandler))
	e.Now = func() time.Time { return now }
	e.tracks = map[string]*track{}

	f := aisFrame() // 4 m/s on 290°
	e.handle(f)
	p.Flush(context.Background())
	require.Equal(t, 1, api.n())

	now = t0.Add(20 * time.Minute) // vessel went silent
	e.setDR(drRequest{"mdti-ais-701000677", true})
	p.Flush(context.Background())
	require.Equal(t, 2, api.n())
	dr := api.calls[1]
	want := geo.Destination(geo.Point{Lat: f.Lat, Lon: f.Lon}, 290, 4*1200)
	assert.InDelta(t, want.Lat, *dr.Location.Position.LatitudeDegrees, 1e-9)
	assert.InDelta(t, want.Lon, *dr.Location.Position.LongitudeDegrees, 1e-9)
	assert.Equal(t, 1, *dr.Tracked.TrackQualityWrapper)
	assert.Equal(t, now.Add(DRExpiry), *dr.ExpiryTime)
	assert.Equal(t, t0, *dr.Tracked.LastMeasurementTime)
	assert.Contains(t, *dr.Description, "AIS DARK")

	now = now.Add(30 * time.Second)
	e.housekeeping() // DR republish keeps the entity alive
	p.Flush(context.Background())
	require.Equal(t, 3, api.n())
	assert.True(t, api.calls[2].Provenance.SourceUpdateTime.After(*dr.Provenance.SourceUpdateTime))

	// A real report ends DR.
	f2 := f
	f2.ObservedAt = now.Add(time.Second)
	e.handle(f2)
	assert.True(t, e.tracks["mdti-ais-701000677"].drSince.IsZero())
}

func TestEngineIgnoresOutOfOrderAndEvicts(t *testing.T) {
	now := t0
	api := &fakeAPI{}
	p := newTestPublisher(api)
	e := NewEngine(mapper(), p, slog.New(slog.DiscardHandler))
	e.Now = func() time.Time { return now }
	e.tracks = map[string]*track{}

	f := aisFrame()
	e.handle(f)
	old := f
	old.ObservedAt = t0.Add(-time.Second)
	e.handle(old)
	assert.Equal(t, 1, p.Pending())
	assert.Equal(t, t0, e.tracks["mdti-ais-701000677"].last.ObservedAt)

	now = t0.Add(31 * time.Minute)
	e.housekeeping()
	assert.Empty(t, e.tracks)
}

func TestEngineClassifyOverlay(t *testing.T) {
	api := &fakeAPI{}
	p := newTestPublisher(api)
	e := NewEngine(mapper(), p, slog.New(slog.DiscardHandler))
	e.Now = func() time.Time { return t0 }
	e.tracks = map[string]*track{}

	f := liveADSBFrame(t)
	e.handle(f)
	p.Flush(context.Background())
	e.setClass(classification{entityID: "mdti-adsb-e0aa11", disp: Lattice.MilViewDispositionDispositionHostile, note: "[MDTI] GHOST · HOSTILE"})
	p.Flush(context.Background())
	require.Equal(t, 2, api.n())
	ent := api.calls[1]
	assert.Equal(t, "SHAPCF---------", *ent.Symbology.MilStd2525C.Sidc, "affiliation follows the C2 verdict")
	assert.True(t, strings.HasPrefix(*ent.Description, "[MDTI] GHOST · HOSTILE · "))
	assert.Equal(t, Lattice.MilViewDispositionDispositionNeutral, *ent.MilView.Disposition,
		"ING never publishes the verdict as disposition: the C2 override carries it")
	assert.Equal(t, f.ObservedAt.Add(time.Millisecond), *ent.Provenance.SourceUpdateTime)

	// Subsequent real reports keep the overlay.
	f2 := f
	f2.ObservedAt = f.ObservedAt.Add(2 * time.Second)
	e.handle(f2)
	p.Flush(context.Background())
	require.Equal(t, 3, api.n())
	assert.Equal(t, "SHAPCF---------", *api.calls[2].Symbology.MilStd2525C.Sidc)
}
