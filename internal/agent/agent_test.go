// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package agent

import (
	"context"
	"errors"
	"log/slog"
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
)

var t0 = time.Date(2026, 10, 21, 22, 0, 0, 0, time.UTC)

type fakeLattice struct {
	mu        sync.Mutex
	targets   map[string]geo.Point
	published []*Lattice.Entity
	updates   []update
	rejectV   map[int]bool // versions the server rejects once
	serverVer int
}

type update struct {
	version int
	status  Lattice.TaskStatusStatus
	code    Lattice.TaskErrorCode
}

func (f *fakeLattice) PublishEntity(_ context.Context, e *Lattice.Entity, _ ...option.RequestOption) (*Lattice.Entity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, e)
	return e, nil
}

func (f *fakeLattice) GetEntity(_ context.Context, r *Lattice.GetEntityRequest, _ ...option.RequestOption) (*Lattice.Entity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.targets[r.EntityID]
	if !ok {
		return nil, core.NewAPIError(404, nil, errors.New("not found"))
	}
	return &Lattice.Entity{EntityID: &r.EntityID, Location: &Lattice.Location{Position: &Lattice.Position{
		LatitudeDegrees: Lattice.Float64(p.Lat), LongitudeDegrees: Lattice.Float64(p.Lon)}}}, nil
}

// ownerFake reports an asset entity last published by a given instance.
type ownerFake struct {
	*fakeLattice
	owner string
	at    time.Time
}

func (f *ownerFake) GetEntity(ctx context.Context, r *Lattice.GetEntityRequest, o ...option.RequestOption) (*Lattice.Entity, error) {
	if r.EntityID != "mdti-asset-sentinel-1" {
		return f.fakeLattice.GetEntity(ctx, r, o...)
	}
	return &Lattice.Entity{EntityID: &r.EntityID, IsLive: Lattice.Bool(true),
		Provenance: &Lattice.Provenance{SourceDescription: Lattice.String(f.owner), SourceUpdateTime: &f.at}}, nil
}

func (f *fakeLattice) UpdateTaskStatus(_ context.Context, r *Lattice.TaskStatusUpdate, _ ...option.RequestOption) (*Lattice.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rejectV[*r.StatusVersion] {
		delete(f.rejectV, *r.StatusVersion)
		f.serverVer = *r.StatusVersion // someone else bumped it
		return nil, core.NewAPIError(409, nil, errors.New("version"))
	}
	u := update{version: *r.StatusVersion, status: *r.NewStatus.Status}
	if r.NewStatus.TaskError != nil {
		u.code = *r.NewStatus.TaskError.Code
	}
	f.updates = append(f.updates, u)
	f.serverVer = *r.StatusVersion
	st := *r.NewStatus.Status
	return &Lattice.Task{Status: &Lattice.TaskStatus{Status: &st}}, nil
}

func (f *fakeLattice) GetTask(_ context.Context, r *Lattice.GetTaskRequest, _ ...option.RequestOption) (*Lattice.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.serverVer
	return &Lattice.Task{Version: &Lattice.TaskVersion{TaskID: &r.TaskID, StatusVersion: &v}}, nil
}

func (f *fakeLattice) statuses() []Lattice.TaskStatusStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Lattice.TaskStatusStatus
	for _, u := range f.updates {
		out = append(out, u.status)
	}
	return out
}

type sim struct {
	now   time.Time
	f     *fakeLattice
	asset *Asset
}

func newSim(t *testing.T, target geo.Point) *sim {
	t.Helper()
	s := &sim{now: t0, f: &fakeLattice{targets: map[string]geo.Point{"mdti-adsb-e3f00d": target}, rejectV: map[int]bool{}}}
	b := DefaultBehaviour()
	b.DwellVID = 20 * time.Second
	s.asset = NewAsset(DefaultAssets()[0], b, s.f, s.f, "mdti", slog.New(slog.DiscardHandler))
	s.asset.Now = func() time.Time { return s.now }
	return s
}

func (s *sim) run(seconds int) {
	for range seconds {
		s.now = s.now.Add(time.Second)
		s.asset.Step(context.Background(), 1)
	}
}

func vidTask(id string, version int) *Lattice.Task {
	sent := Lattice.TaskStatusStatusStatusSent
	return &Lattice.Task{
		Version:       &Lattice.TaskVersion{TaskID: &id, StatusVersion: &version},
		Status:        &Lattice.TaskStatus{Status: &sent},
		Specification: &Lattice.GoogleProtobufAny{Type: Lattice.String("type.googleapis.com/anduril.tasks.v2.VisualId"), ExtraProperties: map[string]any{"objective": map[string]any{"entityId": "mdti-adsb-e3f00d"}}},
	}
}

func TestFullLifecycle(t *testing.T) {
	target := geo.Destination(DefaultAssets()[0].Home, 300, 8*domain.NMToM)
	s := newSim(t, target)
	s.asset.accept(context.Background(), vidTask("t1", 3))
	assert.Equal(t, []Lattice.TaskStatusStatus{"STATUS_ACK", "STATUS_WILCO", "STATUS_EXECUTING"}, s.f.statuses())
	assert.Equal(t, ModeTransit, s.asset.mode)

	s.run(20 * 60)
	got := s.f.statuses()
	require.NotEmpty(t, got)
	assert.Equal(t, Lattice.TaskStatusStatus("STATUS_DONE_OK"), got[len(got)-1])
	for i, u := range s.f.updates {
		assert.Equal(t, 4+i, u.version, "versions start after the delivered one and strictly increase")
	}
	assert.Equal(t, ModeIdle, s.asset.mode, "returned home and landed")
	// Transit at 45 m/s over ≈ 12.4 km, then 20 s on target, then RTB: the asset moved.
	assert.Greater(t, len(s.f.published), 100)
}

func TestTerminalStatusIsFinal(t *testing.T) {
	s := newSim(t, geo.Destination(DefaultAssets()[0].Home, 0, 2000))
	s.asset.accept(context.Background(), vidTask("t1", 0))
	s.run(10 * 60)
	n := len(s.f.updates)
	s.asset.handle(context.Background(), inbox{cancel: "t1"})
	s.run(60)
	assert.Len(t, s.f.updates, n, "nothing is sent after a terminal state")
}

func TestCancel(t *testing.T) {
	s := newSim(t, geo.Destination(DefaultAssets()[0].Home, 0, 20_000))
	s.asset.accept(context.Background(), vidTask("t1", 0))
	s.run(30)
	s.asset.handle(context.Background(), inbox{cancel: "t1"})
	last := s.f.updates[len(s.f.updates)-1]
	assert.Equal(t, Lattice.TaskStatusStatusStatusDoneNotOk, last.status)
	assert.Equal(t, Lattice.TaskErrorCodeErrorCodeCancelled, last.code)
	assert.Equal(t, ModeRTB, s.asset.mode)
}

func TestBusyRejectKeepsCurrentTask(t *testing.T) {
	s := newSim(t, geo.Destination(DefaultAssets()[0].Home, 0, 20_000))
	s.asset.accept(context.Background(), vidTask("t1", 0))
	s.run(5)
	s.asset.accept(context.Background(), vidTask("t2", 0))
	last := s.f.updates[len(s.f.updates)-1]
	assert.Equal(t, Lattice.TaskStatusStatusStatusDoneNotOk, last.status)
	assert.Equal(t, Lattice.TaskErrorCodeErrorCodeRejected, last.code)
	assert.Equal(t, "t1", s.asset.task.id, "still executing the first task")
	assert.Equal(t, ModeTransit, s.asset.mode)
}

func TestTargetLost(t *testing.T) {
	s := newSim(t, geo.Destination(DefaultAssets()[0].Home, 0, 20_000))
	s.asset.accept(context.Background(), vidTask("t1", 0))
	s.f.mu.Lock()
	delete(s.f.targets, "mdti-adsb-e3f00d") // the track expired
	s.f.mu.Unlock()
	s.run(70)
	last := s.f.updates[len(s.f.updates)-1]
	assert.Equal(t, Lattice.TaskStatusStatusStatusDoneNotOk, last.status)
	assert.Equal(t, Lattice.TaskErrorCodeErrorCodeFailed, last.code)
}

func TestUnknownTargetIsInvalid(t *testing.T) {
	s := newSim(t, geo.Point{})
	task := vidTask("t1", 0)
	task.Specification.ExtraProperties = map[string]any{"objective": map[string]any{"entityId": "nope"}}
	s.asset.accept(context.Background(), task)
	assert.Equal(t, []Lattice.TaskStatusStatus{"STATUS_ACK", "STATUS_DONE_NOT_OK"}, s.f.statuses())
	assert.Equal(t, Lattice.TaskErrorCodeErrorCodeInvalid, s.f.updates[1].code)
}

func TestVersionRejectedRetries(t *testing.T) {
	s := newSim(t, geo.Destination(DefaultAssets()[0].Home, 0, 20_000))
	s.f.rejectV[2] = true // WILCO's version collides once
	s.asset.accept(context.Background(), vidTask("t1", 0))
	assert.Equal(t, []Lattice.TaskStatusStatus{"STATUS_ACK", "STATUS_WILCO", "STATUS_EXECUTING"}, s.f.statuses())
	assert.Equal(t, []int{1, 3, 4}, []int{s.f.updates[0].version, s.f.updates[1].version, s.f.updates[2].version})
}

func TestResumeExecutingTaskSkipsAck(t *testing.T) {
	s := newSim(t, geo.Destination(DefaultAssets()[0].Home, 0, 20_000))
	task := vidTask("t1", 7)
	ex := Lattice.TaskStatusStatusStatusExecuting
	task.Status.Status = &ex
	s.asset.accept(context.Background(), task)
	assert.Empty(t, s.f.updates, "a recovered EXECUTING task continues without re-acknowledging")
	assert.Equal(t, ModeTransit, s.asset.mode)
}

func TestParseObjective(t *testing.T) {
	k, id, p, err := parseObjective(&Lattice.GoogleProtobufAny{Type: Lattice.String("type.googleapis.com/anduril.tasks.v2.Investigate"),
		ExtraProperties: map[string]any{"objective": map[string]any{"point": map[string]any{"lla": map[string]any{"lat": -34.6, "lon": -58.7}}}}})
	require.NoError(t, err)
	assert.Equal(t, "RF", k)
	assert.Empty(t, id)
	assert.Equal(t, geo.Point{Lat: -34.6, Lon: -58.7}, *p)
	k, id, _, err = parseObjective(&Lattice.GoogleProtobufAny{Type: Lattice.String("type.googleapis.com/anduril.tasks.v2.Investigate"),
		ExtraProperties: map[string]any{"objective": map[string]any{"entityId": "x"}}})
	require.NoError(t, err)
	assert.Equal(t, "SHADOW", k)
	assert.Equal(t, "x", id)
	_, _, _, err = parseObjective(&Lattice.GoogleProtobufAny{Type: Lattice.String("type.googleapis.com/acme.Strike"),
		ExtraProperties: map[string]any{"objective": map[string]any{"entityId": "x"}}})
	assert.ErrorContains(t, err, "unsupported task type")
}

func TestStepTowardTurnRateAndNoOvershoot(t *testing.T) {
	s := State{Pos: geo.Point{Lat: -34.6, Lon: -58.6}, Heading: 0}
	target := geo.Destination(s.Pos, 180, 10_000) // directly behind
	next := StepToward(s, target, 45, 1)
	assert.InDelta(t, 3, absf(geo.AngleDiff(0, next.Heading)), 1e-9, "turn limited to 3°/s")
	close := State{Pos: s.Pos, Heading: 90}
	end := StepToward(close, geo.Destination(s.Pos, 90, 20), 45, 1)
	assert.InDelta(t, 0, geo.Haversine(end.Pos, geo.Destination(s.Pos, 90, 20)), 0.5, "stops at the target")
}

func TestOrbitConvergesToRadius(t *testing.T) {
	c := geo.Point{Lat: -46, Lon: -60.8}
	s := State{Pos: geo.Destination(c, 0, 4000)}
	for range 120 {
		s = OrbitStep(s, c, 1000, 110, 1)
	}
	assert.InDelta(t, 1000, geo.Haversine(c, s.Pos), 50)
}

func TestAssetEntityIsComplete(t *testing.T) {
	cfg := DefaultAssets()[0]
	e := entity(cfg, State{Pos: cfg.Home, Heading: 90, SpeedMps: 45}, ModeTransit, "TRANSIT", true, "mdti", t0)
	assert.Equal(t, Lattice.OntologyTemplateTemplateAsset, *e.Ontology.Template)
	assert.Equal(t, "SFAPMFQ--------", *e.Symbology.MilStd2525C.Sidc)
	assert.Len(t, e.TaskCatalog.TaskDefinitions, 2)
	assert.Equal(t, Lattice.HealthConnectionStatusConnectionStatusOnline, *e.Health.ConnectionStatus)
	assert.Len(t, e.Sensors.Sensors, 2)
	assert.InDelta(t, 45, *e.Location.VelocityEnu.E, 1e-9)
	for _, s := range e.Sensors.Sensors {
		_, err := Lattice.NewSensorSensorTypeFromString(string(*s.SensorType))
		assert.NoError(t, err)
	}
	_, err := Lattice.NewMilViewNationalityFromString(string(*e.MilView.Nationality))
	assert.NoError(t, err)
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestSingleWriterLock(t *testing.T) {
	f := &ownerFake{fakeLattice: &fakeLattice{targets: map[string]geo.Point{}}, owner: "agent-instance=other", at: t0.Add(-5 * time.Second)}
	a := NewAsset(DefaultAssets()[0], DefaultBehaviour(), f, f, "mdti", slog.New(slog.DiscardHandler))
	a.Instance, a.Now = "me", func() time.Time { return t0 }
	assert.ErrorIs(t, a.CheckExclusive(context.Background()), ErrDuplicateAgent, "another instance published 5 s ago")

	f.at = t0.Add(-time.Minute)
	assert.NoError(t, a.CheckExclusive(context.Background()), "a stale publish (crashed instance) does not block")
	f.owner, f.at = "agent-instance=me", t0
	assert.NoError(t, a.CheckExclusive(context.Background()), "our own publishes are fine")

	// A conflict detected while running stops the asset before it publishes.
	f.owner, f.at = "agent-instance=other", t0
	var stopped error
	a.OnConflict = func(err error) { stopped = err }
	before := len(f.published)
	a.Step(context.Background(), 1)
	assert.ErrorIs(t, stopped, ErrDuplicateAgent)
	assert.Len(t, f.published, before, "does not publish on top of the other instance")
}
