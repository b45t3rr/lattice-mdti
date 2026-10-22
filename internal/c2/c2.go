// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package c2 turns authorised Tier-2 decisions into Lattice actions (SPEC §8):
// a disposition override, an operator annotation, dead-reckoning for dark
// vessels, and an ISR task for the right patrol asset.
package c2

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/option"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
	"github.com/b45t3rr/lattice-mdti/internal/tier2"
	"github.com/b45t3rr/lattice-mdti/internal/viz"
)

// Task specification URLs (verified live against the Sandbox, SPEC §3.4.5).
const (
	SpecVisualID    = "type.googleapis.com/anduril.tasks.v2.VisualId"
	SpecInvestigate = "type.googleapis.com/anduril.tasks.v2.Investigate"
)

// EntityAPI is the subset of the entities client C2 uses.
type EntityAPI interface {
	OverrideEntity(ctx context.Context, request *Lattice.EntityOverride, opts ...option.RequestOption) (*Lattice.Entity, error)
	PublishEntity(ctx context.Context, request *Lattice.Entity, opts ...option.RequestOption) (*Lattice.Entity, error)
}

// TaskAPI is the subset of the tasks client C2 uses.
type TaskAPI interface {
	CreateTask(ctx context.Context, request *Lattice.TaskCreation, opts ...option.RequestOption) (*Lattice.Task, error)
	GetTask(ctx context.Context, request *Lattice.GetTaskRequest, opts ...option.RequestOption) (*Lattice.Task, error)
}

// Annotator is ING: it owns the entities and carries what cannot be overridden.
type Annotator interface {
	Classify(entityID string, disp Lattice.MilViewDisposition, note string)
	StartDeadReckoning(entityID string)
}

// Controller executes decisions. Safe for concurrent use.
type Controller struct {
	Entities    EntityAPI
	Tasks       TaskAPI
	ING         Annotator // nil when ING runs elsewhere
	Assets      []tier2.Asset
	Integration string
	Log         *slog.Logger
	// DetectionOnly marks, classifies and draws the evidence but never
	// dispatches an asset (demo and observation-only deployments).
	DetectionOnly bool

	mu         sync.Mutex
	targetTask map[string]string // target entity → open task id
	assetTask  map[string]string // asset entity → open task id
	alerts     map[string]*alert // target entity → map marker
}

// alert is the C2 map marker for one confirmed anomaly.
type alert struct {
	at       geo.Point
	label    string
	desc     string
	color    viz.RGBA
	category domain.ThreatCategory
	task     string
	taskKind domain.RecommendedTask
	done     bool
	search   *geo.Point // RF search centre
	evidence []viz.Layer
	until    time.Time
}

// Outcome reports what C2 did for one decision.
type Outcome struct {
	Overridden   bool
	Annotated    bool
	DeadReckoned bool
	TaskID       string
	Asset        string
	Skipped      string // why a dispatch did not happen
	Evidence     string // headline of the measured inconsistency (confirmed anomalies)
}

func (c *Controller) init() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.targetTask == nil {
		c.targetTask, c.assetTask, c.alerts = map[string]string{}, map[string]string{}, map[string]*alert{}
	}
}

// Handle executes one decision. Errors are logged and reflected in the outcome;
// one failing action never blocks the others.
func (c *Controller) Handle(ctx context.Context, d domain.TacticalDecision, ev domain.AnomalyEvent, evidencePath string) Outcome {
	c.init()
	var out Outcome
	note := annotation(d, evidencePath)

	var disp Lattice.MilViewDisposition
	if d.Action.Disposition != "" {
		if ld, ok := lattice.Disposition(d.Action.Disposition); ok {
			disp = ld
			if err := c.override(ctx, d.EntityID, ld); err != nil {
				c.Log.Error("disposition override failed", "entity", d.EntityID, "err", lattice.Describe(err))
			} else {
				out.Overridden = true
			}
		}
	}
	if c.ING != nil && (d.Action.Disposition != "" || d.Disposition != "" || d.AnomalyConfirmed) {
		c.ING.Classify(d.EntityID, disp, note)
		out.Annotated = true
	}
	if c.ING != nil && d.AnomalyConfirmed && ev.Type == domain.AnomalyAISDarkEEZ {
		c.ING.StartDeadReckoning(d.EntityID)
		out.DeadReckoned = true
	}
	switch {
	case d.Action.DispatchTask && c.DetectionOnly:
		out.Skipped = "detection-only mode"
	case d.Action.DispatchTask:
		out.TaskID, out.Asset, out.Skipped = c.dispatch(ctx, d, ev, evidencePath)
	}
	if d.AnomalyConfirmed && (d.Action.Disposition != "" || d.Disposition != "") {
		out.Evidence = c.mark(ctx, d, ev, out.TaskID)
	}
	return out
}

// mark draws (or refreshes) the alert marker for a confirmed anomaly and
// returns its evidence headline.
func (c *Controller) mark(ctx context.Context, d domain.TacticalDecision, ev domain.AnomalyEvent, taskID string) string {
	if len(ev.Window) == 0 {
		return ""
	}
	last := ev.Window[len(ev.Window)-1]
	disp := d.Action.Disposition
	if disp == "" {
		disp = d.Disposition
	}
	color := viz.Orange
	if disp == domain.DispositionHostile {
		color = viz.Red
	}
	headline, evidence := Evidence(ev)
	a := &alert{
		at:       geo.Point{Lat: last.Lat, Lon: last.Lon},
		label:    fmt.Sprintf("%s · %s", headline, disp),
		desc:     fmt.Sprintf("%s · %s P=%.2f · %s", headline, d.ThreatCategory, d.ConfidenceScore, d.TacticalReasoning),
		evidence: evidence,
		color:    color,
		category: d.ThreatCategory,
		task:     taskID,
		taskKind: d.RecommendedTask,
		until:    time.Now().Add(20 * time.Minute),
	}
	if ev.Type == domain.AnomalyAISDarkEEZ && d.TargetCoordinates != nil {
		a.at = geo.Point{Lat: d.TargetCoordinates.Lat, Lon: d.TargetCoordinates.Lon}
	}
	if taskID != "" && d.RecommendedTask == domain.TaskRFEmitterLocate && d.TargetCoordinates != nil {
		a.search = &geo.Point{Lat: d.TargetCoordinates.Lat, Lon: d.TargetCoordinates.Lon}
	}
	c.mu.Lock()
	c.alerts[d.EntityID] = a
	c.mu.Unlock()
	c.drawAlert(ctx, d.EntityID, a)
	return headline
}

func (c *Controller) drawAlert(ctx context.Context, entityID string, a *alert) {
	layers := append(viz.Alert(entityID, a.at, a.label, a.desc, a.color), a.evidence...)
	if a.search != nil {
		layers = append(layers, viz.SearchArea(entityID, *a.search, 5000))
	}
	now := time.Now().UTC()
	for _, l := range layers {
		if _, err := c.Entities.PublishEntity(ctx, l.Entity(c.Integration+"-c2", now, a.until)); err != nil {
			c.Log.Warn("alert layer publish failed", "layer", l.ID, "err", lattice.Describe(err))
		}
	}
}

// Run keeps alert markers alive and updates them when their task ends.
func (c *Controller) Run(ctx context.Context) {
	c.init()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	refresh := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		c.mu.Lock()
		pending := map[string]*alert{}
		for id, a := range c.alerts {
			pending[id] = a
		}
		c.mu.Unlock()
		redraw := time.Since(refresh) > time.Minute
		if redraw {
			refresh = time.Now()
		}
		for id, a := range pending {
			changed := false
			if a.task != "" && !a.done {
				t, err := c.Tasks.GetTask(ctx, &Lattice.GetTaskRequest{TaskID: a.task})
				if err == nil && t.Status != nil && t.Status.Status != nil && Terminal(*t.Status.Status) {
					a.done, changed = true, true
					a.label = outcomeLabel(a, *t.Status.Status)
					c.Log.Info("task outcome on map", "target", id, "label", a.label)
				}
			}
			if changed || redraw {
				c.drawAlert(ctx, id, a)
			}
		}
	}
}

// outcomeLabel is the marker label after the ISR task ended. The agent is a
// simulator, so results are stated as simulated where they imply sensing.
func outcomeLabel(a *alert, st Lattice.TaskStatusStatus) string {
	if st != Lattice.TaskStatusStatusStatusDoneOk {
		return fmt.Sprintf("MDTI ALERT · %s · %s ended %s", a.category, shortTask(a.taskKind), st)
	}
	switch a.category {
	case domain.ThreatADSBGhost:
		return "VID COMPLETE (sim) · no aircraft at reported position · GHOST CONFIRMED"
	case domain.ThreatGNSSSpoofing:
		return "RF SEARCH COMPLETE (sim) · spoofing emitter area surveyed"
	case domain.ThreatAISDark, domain.ThreatIUUFishing:
		return "VID COMPLETE (sim) · dark vessel visually identified"
	case domain.ThreatKinematicSpoof:
		return "SHADOW COMPLETE (sim) · real aircraft position confirmed"
	}
	return fmt.Sprintf("%s COMPLETE (sim)", shortTask(a.taskKind))
}

func (c *Controller) override(ctx context.Context, entityID string, d Lattice.MilViewDisposition) error {
	now := time.Now().UTC()
	_, err := c.Entities.OverrideEntity(ctx, &Lattice.EntityOverride{
		EntityID:  entityID,
		FieldPath: "mil_view.disposition", // the only overridable field among those MDTI sets
		Entity:    &Lattice.Entity{EntityID: Lattice.String(entityID), MilView: &Lattice.MilView{Disposition: &d}},
		Provenance: &Lattice.Provenance{
			IntegrationName: Lattice.String(c.Integration + "-c2"), DataType: Lattice.String("tier2-decision"),
			SourceUpdateTime: &now,
		},
	})
	return err
}

// annotation is the operator note carried on the entity description.
func annotation(d domain.TacticalDecision, evidencePath string) string {
	s := "[MDTI] " + d.TacticalReasoning
	if evidencePath != "" {
		s += " · evidence=" + evidencePath
	}
	return s
}

// dispatch creates the ISR task, one open task per target and per asset.
func (c *Controller) dispatch(ctx context.Context, d domain.TacticalDecision, ev domain.AnomalyEvent, evidencePath string) (taskID, asset, skipped string) {
	if id := c.openTask(ctx, c.targetTask, d.EntityID); id != "" {
		return "", "", "target already tasked (" + id + ")"
	}
	a, why := c.pickAsset(ctx, ev.Domain, d.TargetCoordinates)
	if why != "" {
		return "", "", why
	}
	spec, err := Specification(d.RecommendedTask, d.EntityID, d.TargetCoordinates)
	if err != nil {
		return "", a.ID, err.Error()
	}
	desc := fmt.Sprintf("[MDTI] %s · %s · P=%.2f · anomaly=%s", d.RecommendedTask, d.ThreatCategory, d.ConfidenceScore, d.AnomalyID)
	if evidencePath != "" {
		desc += " · evidence=" + evidencePath
	}
	t, err := c.Tasks.CreateTask(ctx, &Lattice.TaskCreation{
		DisplayName:         Lattice.String(fmt.Sprintf("MDTI %s %s", shortTask(d.RecommendedTask), d.EntityID)),
		Description:         Lattice.String(desc),
		Specification:       spec,
		Author:              &Lattice.Principal{System: &Lattice.System{ServiceName: Lattice.String(c.Integration + "-c2")}},
		Relations:           &Lattice.Relations{Assignee: &Lattice.Principal{System: &Lattice.System{EntityID: Lattice.String(a.ID)}}},
		IsExecutedElsewhere: Lattice.Bool(false),
	})
	if err != nil {
		c.Log.Error("task creation failed", "entity", d.EntityID, "asset", a.ID, "err", lattice.Describe(err))
		return "", a.ID, "create failed: " + lattice.Describe(err)
	}
	taskID = *t.Version.TaskID
	c.mu.Lock()
	c.targetTask[d.EntityID], c.assetTask[a.ID] = taskID, taskID
	c.mu.Unlock()
	c.Log.Info("task dispatched", "task", taskID, "asset", a.ID, "target", d.EntityID, "type", d.RecommendedTask)
	return taskID, a.ID, ""
}

// openTask returns the open task tracked under key, forgetting it once terminal.
func (c *Controller) openTask(ctx context.Context, m map[string]string, key string) string {
	c.mu.Lock()
	id := m[key]
	c.mu.Unlock()
	if id == "" {
		return ""
	}
	t, err := c.Tasks.GetTask(ctx, &Lattice.GetTaskRequest{TaskID: id})
	if err == nil && t.Status != nil && t.Status.Status != nil && Terminal(*t.Status.Status) {
		c.mu.Lock()
		delete(m, key)
		c.mu.Unlock()
		return ""
	}
	return id // still open, or unknown (assume open rather than double-task)
}

// Terminal reports whether a task status is final.
func Terminal(s Lattice.TaskStatusStatus) bool {
	switch s {
	case Lattice.TaskStatusStatusStatusDoneOk, Lattice.TaskStatusStatusStatusDoneNotOk, Lattice.TaskStatusStatusStatusReplaced:
		return true
	}
	return false
}

// pickAsset returns the idle asset of the domain closest to the target
// (home position when the target is unknown).
func (c *Controller) pickAsset(ctx context.Context, d domain.Domain, at *domain.Coordinates) (tier2.Asset, string) {
	var best tier2.Asset
	bestD, seen := math.Inf(1), 0
	for _, a := range c.Assets {
		if a.Domain != d {
			continue
		}
		seen++
		if c.openTask(ctx, c.assetTask, a.ID) != "" {
			continue
		}
		dist := 0.0
		if at != nil {
			dist = geo.Haversine(geo.Point{Lat: a.Lat, Lon: a.Lon}, geo.Point{Lat: at.Lat, Lon: at.Lon})
		}
		if dist < bestD {
			best, bestD = a, dist
		}
	}
	switch {
	case seen == 0:
		return best, "no asset for domain " + string(d)
	case best.ID == "":
		return best, "all assets busy"
	}
	return best, ""
}

// Specification builds the task specification (SPEC §3.4.5): VisualId and
// Investigate reference the target entity; RF emitter location searches a point.
func Specification(task domain.RecommendedTask, targetID string, at *domain.Coordinates) (*Lattice.GoogleProtobufAny, error) {
	entity := map[string]any{"objective": map[string]any{"entityId": targetID}}
	switch task {
	case domain.TaskVisualIdentification:
		return &Lattice.GoogleProtobufAny{Type: Lattice.String(SpecVisualID), ExtraProperties: entity}, nil
	case domain.TaskInterceptAndShadow:
		return &Lattice.GoogleProtobufAny{Type: Lattice.String(SpecInvestigate), ExtraProperties: entity}, nil
	case domain.TaskRFEmitterLocate:
		if at == nil {
			return nil, fmt.Errorf("RF emitter location needs target coordinates")
		}
		lla := map[string]any{"lat": at.Lat, "lon": at.Lon}
		if at.AltHaeM != nil {
			lla["alt"] = *at.AltHaeM
		}
		return &Lattice.GoogleProtobufAny{Type: Lattice.String(SpecInvestigate),
			ExtraProperties: map[string]any{"objective": map[string]any{"point": map[string]any{"lla": lla}}}}, nil
	}
	return nil, fmt.Errorf("task %q is not dispatchable", task)
}

func shortTask(t domain.RecommendedTask) string {
	switch t {
	case domain.TaskVisualIdentification:
		return "VID"
	case domain.TaskInterceptAndShadow:
		return "SHADOW"
	case domain.TaskRFEmitterLocate:
		return "RF-LOCATE"
	}
	return string(t)
}
