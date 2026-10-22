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
	"sync"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/option"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
	"github.com/b45t3rr/lattice-mdti/internal/tier2"
)

// Task specification URLs (verified live against the Sandbox, SPEC §3.4.5).
const (
	SpecVisualID    = "type.googleapis.com/anduril.tasks.v2.VisualId"
	SpecInvestigate = "type.googleapis.com/anduril.tasks.v2.Investigate"
)

// EntityAPI is the subset of the entities client C2 uses.
type EntityAPI interface {
	OverrideEntity(ctx context.Context, request *Lattice.EntityOverride, opts ...option.RequestOption) (*Lattice.Entity, error)
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

	mu         sync.Mutex
	targetTask map[string]string // target entity → open task id
	assetTask  map[string]string // asset entity → open task id
}

// Outcome reports what C2 did for one decision.
type Outcome struct {
	Overridden   bool
	Annotated    bool
	DeadReckoned bool
	TaskID       string
	Asset        string
	Skipped      string // why a dispatch did not happen
}

func (c *Controller) init() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.targetTask == nil {
		c.targetTask, c.assetTask = map[string]string{}, map[string]string{}
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
	if d.Action.DispatchTask {
		out.TaskID, out.Asset, out.Skipped = c.dispatch(ctx, d, ev, evidencePath)
	}
	return out
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
	a, ok := c.assetFor(ev.Domain)
	if !ok {
		return "", "", "no asset for domain " + string(ev.Domain)
	}
	if id := c.openTask(ctx, c.targetTask, d.EntityID); id != "" {
		return "", a.ID, "target already tasked (" + id + ")"
	}
	if id := c.openTask(ctx, c.assetTask, a.ID); id != "" {
		return "", a.ID, "asset busy (" + id + ")"
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

func (c *Controller) assetFor(d domain.Domain) (tier2.Asset, bool) {
	for _, a := range c.Assets {
		if a.Domain == d {
			return a, true
		}
	}
	return tier2.Asset{}, false
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
