// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package c2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/core"
	"github.com/anduril/lattice-sdk-go/v5/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/tier2"
)

type fakeLattice struct {
	mu        sync.Mutex
	overrides []*Lattice.EntityOverride
	tasks     []*Lattice.TaskCreation
	status    map[string]Lattice.TaskStatusStatus
	failTask  bool
	published []*Lattice.Entity
}

func (f *fakeLattice) OverrideEntity(_ context.Context, r *Lattice.EntityOverride, _ ...option.RequestOption) (*Lattice.Entity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overrides = append(f.overrides, r)
	return r.Entity, nil
}

func (f *fakeLattice) PublishEntity(_ context.Context, e *Lattice.Entity, _ ...option.RequestOption) (*Lattice.Entity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, e)
	return e, nil
}

func (f *fakeLattice) CreateTask(_ context.Context, r *Lattice.TaskCreation, _ ...option.RequestOption) (*Lattice.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failTask {
		return nil, core.NewAPIError(400, nil, errors.New("bad spec"))
	}
	f.tasks = append(f.tasks, r)
	id := fmt.Sprintf("task-%d", len(f.tasks))
	if f.status == nil {
		f.status = map[string]Lattice.TaskStatusStatus{}
	}
	f.status[id] = Lattice.TaskStatusStatusStatusSent
	return &Lattice.Task{Version: &Lattice.TaskVersion{TaskID: &id}}, nil
}

func (f *fakeLattice) GetTask(_ context.Context, r *Lattice.GetTaskRequest, _ ...option.RequestOption) (*Lattice.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status[r.TaskID]
	return &Lattice.Task{Status: &Lattice.TaskStatus{Status: &st}}, nil
}

type fakeING struct {
	classified []string
	activity   []string
	disp       []Lattice.MilViewDisposition
	dr         []string
}

func (f *fakeING) Classify(id string, d Lattice.MilViewDisposition, note, activity string) {
	f.classified = append(f.classified, id+"|"+note)
	f.activity = append(f.activity, activity)
	f.disp = append(f.disp, d)
}
func (f *fakeING) StartDeadReckoning(id string) { f.dr = append(f.dr, id) }

func controller(f *fakeLattice, ing *fakeING) *Controller {
	return &Controller{Entities: f, Tasks: f, ING: ing, Integration: "mdti", Log: slog.New(slog.DiscardHandler),
		Assets: []tier2.Asset{
			{ID: "mdti-asset-sentinel-1", Domain: domain.DomainAir, SpeedMps: 45},
			{ID: "mdti-asset-trident-2", Domain: domain.DomainSea, SpeedMps: 110},
		}}
}

func ghostDecision() (domain.TacticalDecision, domain.AnomalyEvent) {
	d := domain.TacticalDecision{AnomalyID: "a1", EntityID: "mdti-adsb-e3f00d", AnomalyConfirmed: true,
		Disposition: domain.DispositionHostile, ConfidenceScore: 0.91, ThreatCategory: domain.ThreatADSBGhost,
		TacticalReasoning: "GHOST_TRACK near SADM", RecommendedTask: domain.TaskVisualIdentification,
		TargetCoordinates: &domain.Coordinates{Lat: -34.65, Lon: -58.68},
		Action:            domain.Action{Disposition: domain.DispositionHostile, DispatchTask: true}}
	return d, domain.AnomalyEvent{ID: "a1", EntityID: d.EntityID, Domain: domain.DomainAir, Type: domain.AnomalyGhostTrack}
}

func TestHandleOverridesAnnotatesAndTasks(t *testing.T) {
	f, ing := &fakeLattice{}, &fakeING{}
	c := controller(f, ing)
	d, ev := ghostDecision()
	out := c.Handle(context.Background(), d, ev, "mdti/evidence/x/a1.json")

	assert.True(t, out.Overridden)
	require.Len(t, f.overrides, 1)
	o := f.overrides[0]
	assert.Equal(t, "mil_view.disposition", o.FieldPath)
	assert.Equal(t, Lattice.MilViewDispositionDispositionHostile, *o.Entity.MilView.Disposition)

	require.Len(t, ing.classified, 1)
	assert.Equal(t, "mdti-adsb-e3f00d|[MDTI] GHOST_TRACK near SADM · evidence=mdti/evidence/x/a1.json", ing.classified[0])
	assert.Equal(t, Lattice.MilViewDispositionDispositionHostile, ing.disp[0])

	assert.Equal(t, "mdti-asset-sentinel-1", out.Asset)
	assert.NotEmpty(t, out.TaskID)
	require.Len(t, f.tasks, 1)
	tc := f.tasks[0]
	assert.Equal(t, SpecVisualID, *tc.Specification.Type)
	assert.Equal(t, map[string]any{"objective": map[string]any{"entityId": "mdti-adsb-e3f00d"}}, tc.Specification.ExtraProperties)
	assert.Equal(t, "mdti-asset-sentinel-1", *tc.Relations.Assignee.System.EntityID)
	assert.Equal(t, "mdti-c2", *tc.Author.System.ServiceName)
	assert.False(t, *tc.IsExecutedElsewhere)
	assert.Contains(t, *tc.Description, "evidence=mdti/evidence/x/a1.json")
}

func TestOneOpenTaskPerTargetAndAsset(t *testing.T) {
	f := &fakeLattice{}
	c := controller(f, &fakeING{})
	d, ev := ghostDecision()
	first := c.Handle(context.Background(), d, ev, "")
	second := c.Handle(context.Background(), d, ev, "")
	assert.Empty(t, second.TaskID)
	assert.Contains(t, second.Skipped, "target already tasked")

	d2, ev2 := ghostDecision()
	d2.EntityID, ev2.EntityID = "mdti-adsb-e0c0de", "mdti-adsb-e0c0de"
	busy := c.Handle(context.Background(), d2, ev2, "")
	assert.Contains(t, busy.Skipped, "all assets busy")

	f.mu.Lock()
	f.status[first.TaskID] = Lattice.TaskStatusStatusStatusDoneOk // the agent finished
	f.mu.Unlock()
	free := c.Handle(context.Background(), d2, ev2, "")
	assert.NotEmpty(t, free.TaskID, "a terminal task frees the asset")
	assert.Len(t, f.tasks, 2)
}

func TestSupervisedDecisionDoesNotDispatch(t *testing.T) {
	f, ing := &fakeLattice{}, &fakeING{}
	d, ev := ghostDecision()
	d.Action = domain.Action{Disposition: domain.DispositionSuspect}
	out := controller(f, ing).Handle(context.Background(), d, ev, "")
	assert.Empty(t, f.tasks)
	assert.Empty(t, out.TaskID)
	assert.Equal(t, Lattice.MilViewDispositionDispositionSuspicious, *f.overrides[0].Entity.MilView.Disposition, "SUSPECT → DISPOSITION_SUSPICIOUS")
}

func TestAdvisoryAnnotatesOnly(t *testing.T) {
	f, ing := &fakeLattice{}, &fakeING{}
	d, ev := ghostDecision()
	d.Action = domain.Action{}
	controller(f, ing).Handle(context.Background(), d, ev, "")
	assert.Empty(t, f.overrides)
	assert.Empty(t, f.tasks)
	require.Len(t, ing.classified, 1, "the recommendation is still visible to the operator")
	assert.Empty(t, ing.disp[0], "symbology unchanged without an applied disposition")
}

func TestDarkVesselStartsDeadReckoning(t *testing.T) {
	f, ing := &fakeLattice{}, &fakeING{}
	d := domain.TacticalDecision{EntityID: "mdti-ais-200000001", AnomalyConfirmed: true, Action: domain.Action{Disposition: domain.DispositionSuspect}}
	out := controller(f, ing).Handle(context.Background(), d, domain.AnomalyEvent{Domain: domain.DomainSea, Type: domain.AnomalyAISDarkEEZ}, "")
	assert.True(t, out.DeadReckoned)
	assert.Equal(t, []string{"mdti-ais-200000001"}, ing.dr)
}

func TestSpecification(t *testing.T) {
	s, err := Specification(domain.TaskRFEmitterLocate, "x", &domain.Coordinates{Lat: -34.6, Lon: -58.7, AltHaeM: domain.Ptr(1500.0)})
	require.NoError(t, err)
	assert.Equal(t, SpecInvestigate, *s.Type)
	assert.Equal(t, map[string]any{"objective": map[string]any{"point": map[string]any{"lla": map[string]any{"lat": -34.6, "lon": -58.7, "alt": 1500.0}}}}, s.ExtraProperties)
	s, _ = Specification(domain.TaskInterceptAndShadow, "x", nil)
	assert.Equal(t, SpecInvestigate, *s.Type)
	_, err = Specification(domain.TaskRFEmitterLocate, "x", nil)
	assert.Error(t, err)
	_, err = Specification(domain.TaskNone, "x", nil)
	assert.Error(t, err)
}

func TestTaskFailureIsReported(t *testing.T) {
	f := &fakeLattice{failTask: true}
	d, ev := ghostDecision()
	out := controller(f, &fakeING{}).Handle(context.Background(), d, ev, "")
	assert.Contains(t, out.Skipped, "create failed")
	assert.True(t, out.Overridden, "a failed task does not undo the override")
}

func TestAlertMarkerAndOutcome(t *testing.T) {
	f := &fakeLattice{}
	c := controller(f, &fakeING{})
	d, ev := ghostDecision()
	ev.Window = []domain.Frame{{Lat: -34.61, Lon: -58.78}}
	out := c.Handle(context.Background(), d, ev, "")
	require.Len(t, f.published, 3, "alert area + hazard point + first-fix evidence")
	assert.Contains(t, out.Evidence, "GHOST TRACK · first seen", "the outcome carries the headline for logs")
	ing := c.ING.(*fakeING)
	require.NotEmpty(t, ing.activity)
	assert.Equal(t, out.Evidence, ing.activity[len(ing.activity)-1], "ING shows the same headline in the track panel")
	assert.Equal(t, "mdti-alert-area-mdti-adsb-e3f00d", *f.published[0].EntityID)
	assert.Contains(t, *f.published[1].Aliases.Name, "GHOST TRACK · first seen")
	assert.Contains(t, *f.published[1].Aliases.Name, "· HOSTILE")
	assert.Contains(t, *f.published[1].Description, "ADSB_GHOST_INJECTION P=0.91")
	assert.Equal(t, "mdti-evidence-spawn-mdti-adsb-e3f00d", *f.published[2].EntityID)

	f.mu.Lock()
	f.status[out.TaskID] = Lattice.TaskStatusStatusStatusDoneOk
	f.mu.Unlock()
	a := c.alerts["mdti-adsb-e3f00d"]
	assert.Equal(t, "VID COMPLETE (sim) · no aircraft at reported position · GHOST CONFIRMED",
		outcomeLabel(a, Lattice.TaskStatusStatusStatusDoneOk))
}

func TestDetectionOnlyNeverDispatches(t *testing.T) {
	f := &fakeLattice{}
	c := controller(f, &fakeING{})
	c.DetectionOnly = true
	d, ev := ghostDecision()
	require.True(t, d.Action.DispatchTask)
	out := c.Handle(context.Background(), d, ev, "")
	assert.Empty(t, out.TaskID)
	assert.Equal(t, "detection-only mode", out.Skipped)
	assert.True(t, out.Overridden, "the track is still classified")
	assert.Empty(t, f.tasks, "no task reaches Lattice")
}

func TestEvidenceHeadlines(t *testing.T) {
	win := []domain.Frame{{Lat: -35.0, Lon: -58.2}, {Lat: -34.85, Lon: -58.0}}
	h, layers := Evidence(domain.AnomalyEvent{EntityID: "e", Type: domain.AnomalyKinematicImpossible, Window: win,
		Metrics: map[string]float64{"jump_m": 22224, "dt_s": 2, "implied_speed_mps": 11112}})
	assert.Equal(t, "IMPOSSIBLE KINEMATICS · 22.2 km in 2 s · 11,112 m/s (Mach 33)", h)
	require.Len(t, layers, 1)
	assert.NotNil(t, layers[0].Shape.Line, "the jump is drawn")

	h, _ = Evidence(domain.AnomalyEvent{Type: domain.AnomalyBaroGNSSDivergence,
		Metrics: map[string]float64{"delta_alt_ft": 1850, "baseline_ft": 250, "deviation_ft": 1600}})
	assert.Equal(t, "GNSS/BARO MISMATCH · GNSS altitude +1850 ft from baro (+1600 ft off its baseline)", h)

	h, _ = Evidence(domain.AnomalyEvent{Type: domain.AnomalyAISDarkEEZ, Metrics: map[string]float64{"gap_s": 190, "signed_dist_nm": -3.9}})
	assert.Equal(t, "AIS SILENT 3m10s · last report 3.9 NM inside the 200 NM line", h)

	h, _ = Evidence(domain.AnomalyEvent{Type: domain.AnomalyAISDarkEEZ, Metrics: map[string]float64{"gap_s": 192, "signed_dist_nm": 5, "dr_inside": 1}})
	assert.Equal(t, "AIS SILENT 3m12s · last report 5.0 NM outside the 200 NM line · dead-reckoned position now inside the EEZ", h)

	h, _ = Evidence(domain.AnomalyEvent{Type: domain.AnomalyAISDarkEEZ, Metrics: map[string]float64{
		"gap_s": 192, "signed_dist_nm": 5, "dr_inside": 0, "inward_angle_deg": 12, "course_change_deg": 70}})
	assert.Equal(t, "AIS SILENT 3m12s · last report 5.0 NM outside the 200 NM line · heading into the EEZ (12° off the inward normal) · turned 70° before going silent", h)
}

func TestDarkVesselAlertSitsOnLastReport(t *testing.T) {
	f := &fakeLattice{}
	c := controller(f, &fakeING{})
	d, ev := ghostDecision()
	ev.Type = domain.AnomalyAISDarkEEZ
	ev.Window = []domain.Frame{{Lat: -46.37, Lon: -60.80}}
	d.TargetCoordinates = &domain.Coordinates{Lat: -46.20, Lon: -61.10} // intercept point, inside the EEZ
	c.Handle(context.Background(), d, ev, "")
	a := c.alerts[d.EntityID]
	require.NotNil(t, a)
	assert.Equal(t, geo.Point{Lat: -46.37, Lon: -60.80}, a.at)
}

func TestPicksNearestIdleAsset(t *testing.T) {
	f := &fakeLattice{}
	c := controller(f, &fakeING{})
	c.Assets = append(c.Assets, tier2.Asset{ID: "mdti-asset-sentinel-2", Domain: domain.DomainAir, Lat: -34.82, Lon: -58.54, SpeedMps: 45})
	c.Assets[0].Lat, c.Assets[0].Lon = -34.68, -58.64
	d, ev := ghostDecision()
	d.TargetCoordinates = &domain.Coordinates{Lat: -34.85, Lon: -58.50} // next to Ezeiza
	out := c.Handle(context.Background(), d, ev, "")
	assert.Equal(t, "mdti-asset-sentinel-2", out.Asset)
}
