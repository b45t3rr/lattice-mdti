// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package sim

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/config"
	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
	"github.com/b45t3rr/lattice-mdti/internal/tier2"
)

// The sandbox is exercised only through the official SDKs, exactly as the
// MDTI binaries use it.
func setup(t *testing.T) (*Server, *lattice.Clients, string) {
	t.Helper()
	s := New(slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	c, err := lattice.New(config.Lattice{Endpoint: srv.URL, EnvironmentToken: "offline", SandboxesToken: "offline", IntegrationName: "mdti"})
	require.NoError(t, err)
	return s, c, srv.URL
}

func entity(id string, expiry time.Time) *Lattice.Entity {
	now := time.Now().UTC()
	neutral := Lattice.MilViewDispositionDispositionNeutral
	return &Lattice.Entity{
		EntityID: Lattice.String(id), IsLive: Lattice.Bool(true), ExpiryTime: &expiry,
		Aliases: &Lattice.Aliases{Name: Lattice.String("TEST1")}, MilView: &Lattice.MilView{Disposition: &neutral},
		Location:   &Lattice.Location{Position: &Lattice.Position{LatitudeDegrees: Lattice.Float64(-34.7), LongitudeDegrees: Lattice.Float64(-58.5)}},
		Provenance: &Lattice.Provenance{IntegrationName: Lattice.String("mdti"), DataType: Lattice.String("adsb"), SourceUpdateTime: &now},
	}
}

func TestEntityPublishOverrideExpiry(t *testing.T) {
	s, c, _ := setup(t)
	ctx := context.Background()
	_, err := c.Unary.Entities.PublishEntity(ctx, entity("mdti-adsb-abc123", time.Now().Add(time.Minute)))
	require.NoError(t, err)

	bad := entity("mdti-adsb-bad", time.Now().Add(time.Minute))
	bad.Provenance = nil
	_, err = c.Unary.Entities.PublishEntity(ctx, bad)
	assert.Equal(t, 400, lattice.StatusCode(err), "missing provenance is rejected like the Sandbox does")

	hostile := Lattice.MilViewDispositionDispositionHostile
	_, err = c.Unary.Entities.OverrideEntity(ctx, &Lattice.EntityOverride{EntityID: "mdti-adsb-abc123", FieldPath: "mil_view.disposition",
		Entity: &Lattice.Entity{MilView: &Lattice.MilView{Disposition: &hostile}}})
	require.NoError(t, err)
	_, err = c.Unary.Entities.OverrideEntity(ctx, &Lattice.EntityOverride{EntityID: "mdti-adsb-abc123", FieldPath: "description",
		Entity: &Lattice.Entity{Description: Lattice.String("x")}})
	assert.Equal(t, 400, lattice.StatusCode(err), "only mil_view.disposition is overridable")

	// A later publish (ING heartbeat) does not clear the override.
	_, err = c.Unary.Entities.PublishEntity(ctx, entity("mdti-adsb-abc123", time.Now().Add(time.Minute)))
	require.NoError(t, err)
	e, err := c.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: "mdti-adsb-abc123"})
	require.NoError(t, err)
	assert.Equal(t, hostile, *e.MilView.Disposition)

	_, err = c.Unary.Entities.RemoveEntityOverride(ctx, &Lattice.RemoveEntityOverrideRequest{EntityID: "mdti-adsb-abc123", FieldPath: "mil_view.disposition"})
	require.NoError(t, err)
	e, err = c.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: "mdti-adsb-abc123"})
	require.NoError(t, err)
	assert.Equal(t, Lattice.MilViewDispositionDispositionNeutral, *e.MilView.Disposition)

	s.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	_, err = c.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: "mdti-adsb-abc123"})
	assert.Equal(t, 404, lattice.StatusCode(err), "expired entities disappear")
}

func TestTaskLifecycleOverAgentStream(t *testing.T) {
	s, c, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hb := 100
	stream, err := c.Stream.Tasks.StreamAsAgent(ctx, &Lattice.AgentStreamRequest{
		AgentSelector: &Lattice.EntityIDsSelector{EntityIDs: []string{"mdti-asset-sentinel-1"}}, HeartbeatIntervalMs: &hb})
	require.NoError(t, err)
	defer stream.Close()
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.agents) == 1 }, 2*time.Second, 10*time.Millisecond)

	task, err := c.Unary.Tasks.CreateTask(ctx, &Lattice.TaskCreation{
		DisplayName:   Lattice.String("VID mdti-adsb-e3f00d"),
		Specification: &Lattice.GoogleProtobufAny{Type: Lattice.String("type.googleapis.com/anduril.tasks.v2.VisualId")},
		Relations:     &Lattice.Relations{Assignee: &Lattice.Principal{System: &Lattice.System{EntityID: Lattice.String("mdti-asset-sentinel-1")}}},
	})
	require.NoError(t, err)
	id := *task.Version.TaskID
	assert.Equal(t, Lattice.TaskStatusStatusStatusSent, *task.Status.Status)

	var got *Lattice.Task
	for got == nil {
		msg, err := stream.Recv()
		require.NoError(t, err)
		if msg.AgentRequest != nil && msg.AgentRequest.ExecuteRequest != nil {
			got = msg.AgentRequest.ExecuteRequest.Task
		}
	}
	assert.Equal(t, id, *got.Version.TaskID)

	update := func(v int, st Lattice.TaskStatusStatus) error {
		_, err := c.Unary.Tasks.UpdateTaskStatus(ctx, &Lattice.TaskStatusUpdate{TaskID: id, StatusVersion: &v,
			NewStatus: &Lattice.TaskStatus{Status: &st}})
		return err
	}
	require.NoError(t, update(2, Lattice.TaskStatusStatusStatusAck))
	assert.Error(t, update(2, Lattice.TaskStatusStatusStatusWilco), "status versions must strictly increase")
	require.NoError(t, update(3, Lattice.TaskStatusStatusStatusWilco))
	require.NoError(t, update(4, Lattice.TaskStatusStatusStatusExecuting))
	require.NoError(t, update(5, Lattice.TaskStatusStatusStatusDoneOk))
	assert.Error(t, update(6, Lattice.TaskStatusStatusStatusExecuting), "terminal states are final")

	cur, err := c.Unary.Tasks.GetTask(ctx, &Lattice.GetTaskRequest{TaskID: id})
	require.NoError(t, err)
	assert.Equal(t, Lattice.TaskStatusStatusStatusDoneOk, *cur.Status.Status)
	assert.Equal(t, 5, *cur.Version.StatusVersion)
	assert.NotNil(t, cur.Status.StartTime)

	st := Lattice.TaskQueryStatusFilterStatus("STATUS_DONE_OK")
	res, err := c.Unary.Tasks.QueryTasks(ctx, &Lattice.TaskQuery{StatusFilter: &Lattice.TaskQueryStatusFilter{Status: &st}})
	require.NoError(t, err)
	assert.Len(t, res.Tasks, 1)

	snap := s.Snapshot()
	require.Len(t, snap.Tasks, 1)
	assert.Equal(t, "STATUS_DONE_OK", snap.Tasks[0].Status)
}

func TestObjects(t *testing.T) {
	_, c, _ := setup(t)
	ctx := context.Background()
	md, err := c.Unary.Objects.UploadObject(ctx, "mdti/evidence/x/1.json", bytes.NewReader([]byte(`{"ok":true}`)))
	require.NoError(t, err)
	assert.Equal(t, int64(11), md.SizeBytes)
	r, err := c.Unary.Objects.GetObject(ctx, &Lattice.GetObjectRequest{ObjectPath: "mdti/evidence/x/1.json"})
	require.NoError(t, err)
	b, _ := io.ReadAll(r)
	assert.JSONEq(t, `{"ok":true}`, string(b))
}

// The Decisions stub drives the real Tier-2 engine and policy end to end.
func TestDecisionsStubThroughTier2(t *testing.T) {
	_, _, url := setup(t)
	client := openai.NewClient(option.WithBaseURL(url+"/v1"), option.WithAPIKey("offline"))
	pol, err := tier2.LoadPolicy("../../data/tier2/policy.yaml")
	require.NoError(t, err)
	zones, err := geo.LoadZones("../../data/geo/zones.yaml")
	require.NoError(t, err)
	eng := &tier2.Engine{API: &client.Decisions, Policy: pol, Model: "gpt-6-luna", Autonomy: "autonomous", Zones: zones,
		Concurrency: 1, PerMinute: 600, Log: slog.New(slog.DiscardHandler)}

	now := time.Now()
	sadm := geo.Point{Lat: -34.6763, Lon: -58.6428}
	p := geo.Destination(sadm, 300, 8*domain.NMToM)
	var win []domain.Frame
	for i := range 3 {
		win = append(win, domain.Frame{EntityKey: "adsb:e3f00d", Domain: domain.DomainAir, ObservedAt: now.Add(time.Duration(i-2) * time.Second),
			Lat: p.Lat, Lon: p.Lon, AltBaroFt: domain.Ptr(5000.0), AltGeomFt: domain.Ptr(5150.0), GroundSpeedMps: domain.Ptr(108.0),
			TrackDeg: domain.Ptr(120.0), OnGround: domain.Ptr(false), Category: "A2"})
		p = geo.Destination(p, 120, 108)
	}
	ev := domain.AnomalyEvent{ID: "t-1", EntityID: "mdti-adsb-e3f00d", EntityKey: "adsb:e3f00d", Domain: domain.DomainAir,
		Type: domain.AnomalyGhostTrack, Severity: 4, Rule: "D3 ghost spawn", Escalate: true, Window: win,
		Metrics:    map[string]float64{"dist_ref_nm": 8, "alt_agl_ft": 4905},
		Geo:        domain.GeoContext{NearestAerodrome: "SADM", DistToAerodromeNM: 7.9, CoverageEdgeDistNM: domain.Ptr(47.0)},
		DetectedAt: now}
	d := eng.Decide(context.Background(), ev).Decision
	assert.Empty(t, d.Audit.Fallback)
	assert.Equal(t, domain.DispositionHostile, d.Disposition)
	assert.Equal(t, domain.ThreatADSBGhost, d.ThreatCategory)
	assert.Equal(t, domain.TaskVisualIdentification, d.RecommendedTask)

	ev.Type, ev.Rule = domain.AnomalyNavIntegrityDrop, "D5 nav integrity"
	d = eng.Decide(context.Background(), ev).Decision
	assert.False(t, d.AnomalyConfirmed, "anything without a recorded answer is treated as benign")
}
