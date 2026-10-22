// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/option"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

// EntityAPI is the subset of the entities client the agent uses.
type EntityAPI interface {
	PublishEntity(ctx context.Context, request *Lattice.Entity, opts ...option.RequestOption) (*Lattice.Entity, error)
	GetEntity(ctx context.Context, request *Lattice.GetEntityRequest, opts ...option.RequestOption) (*Lattice.Entity, error)
}

// TaskAPI is the subset of the tasks client the agent uses.
type TaskAPI interface {
	UpdateTaskStatus(ctx context.Context, request *Lattice.TaskStatusUpdate, opts ...option.RequestOption) (*Lattice.Task, error)
	GetTask(ctx context.Context, request *Lattice.GetTaskRequest, opts ...option.RequestOption) (*Lattice.Task, error)
}

// Behaviour holds mission parameters (SPEC §8.5–8.6).
type Behaviour struct {
	DwellVID, DwellShadow, DwellRF time.Duration // time with the target inside sensor range
	SensorRangeAir, SensorRangeSea float64       // metres: EO/IR identification range
	OrbitRadius                    float64       // metres
	TargetRefresh                  time.Duration
	TargetLostAfter                time.Duration
	MaxTaskDuration                time.Duration
}

// DefaultBehaviour returns SPEC defaults.
func DefaultBehaviour() Behaviour {
	return Behaviour{
		DwellVID: 120 * time.Second, DwellShadow: 600 * time.Second, DwellRF: 300 * time.Second,
		SensorRangeAir: 3000, SensorRangeSea: 5000, OrbitRadius: 1000,
		TargetRefresh: 5 * time.Second, TargetLostAfter: 60 * time.Second, MaxTaskDuration: 45 * time.Minute,
	}
}

type inbox struct {
	execute  *Lattice.Task
	cancel   string
	complete string
}

// activeTask is the task an asset is executing.
type activeTask struct {
	id       string
	version  int
	kind     string // VID | SHADOW | RF
	targetID string // "" for point objectives

	fix        geo.Point // last known target position
	fixAt      time.Time
	vel        [2]float64 // target ENU velocity (m/s) for prediction
	lastSeen   time.Time
	lastFetch  time.Time
	started    time.Time
	inRange    time.Duration
	terminated bool
}

// Asset runs one patrol asset: its kinematics, its task state machine and its
// entity heartbeat. All state is owned by Run's goroutine.
type Asset struct {
	Cfg         AssetConfig
	Behaviour   Behaviour
	Entities    EntityAPI
	Tasks       TaskAPI
	Integration string
	Log         *slog.Logger
	Now         func() time.Time

	Inbox chan inbox

	// Instance identifies this process in the asset's provenance. Two agents
	// driving the same asset would fight over its position and its tasks'
	// status versions (found live), so each asset has a single writer.
	Instance   string
	OnConflict func(error)
	lastCheck  time.Time

	state       State
	mode        Mode
	task        *activeTask
	lastPublish time.Time
	status      string
}

// NewAsset builds an asset parked at home.
func NewAsset(cfg AssetConfig, b Behaviour, ent EntityAPI, tasks TaskAPI, integration string, log *slog.Logger) *Asset {
	return &Asset{
		Cfg: cfg, Behaviour: b, Entities: ent, Tasks: tasks, Integration: integration,
		Log: log.With("asset", cfg.Name), Inbox: make(chan inbox, 16),
		state: State{Pos: cfg.Home}, mode: ModeIdle, status: "IDLE at home",
	}
}

func (a *Asset) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// ErrDuplicateAgent means another live process is driving the same asset.
var ErrDuplicateAgent = errors.New("another agent instance is driving this asset")

// exclusiveWindow: a foreign publish this recent means the other instance is alive.
const exclusiveWindow = 15 * time.Second

// CheckExclusive returns ErrDuplicateAgent when the asset entity was published
// by a different instance within exclusiveWindow.
func (a *Asset) CheckExclusive(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e, err := a.Entities.GetEntity(cctx, &Lattice.GetEntityRequest{EntityID: a.Cfg.ID})
	if err != nil || e.Provenance == nil {
		return nil // not published yet (or unreadable): nobody else owns it
	}
	owner := deref(e.Provenance.SourceDescription)
	if owner == "" || owner == a.instanceTag() || e.Provenance.SourceUpdateTime == nil {
		return nil
	}
	if age := a.now().Sub(*e.Provenance.SourceUpdateTime); age < exclusiveWindow && deref(e.IsLive) {
		return fmt.Errorf("%w: %s (%s, last publish %s ago)", ErrDuplicateAgent, a.Cfg.Name, owner, age.Round(time.Second))
	}
	return nil
}

func (a *Asset) instanceTag() string { return "agent-instance=" + a.Instance }

// Run ticks at 1 Hz until ctx ends, then publishes the asset offline.
func (a *Asset) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	a.publish(ctx, true)
	for {
		select {
		case <-ctx.Done():
			off, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if a.task != nil && !a.task.terminated {
				a.finish(off, Lattice.TaskStatusStatusStatusDoneNotOk, errCode(Lattice.TaskErrorCodeErrorCodeCancelled, "agent shutting down"))
			}
			offline := entity(a.Cfg, a.state, a.mode, "OFFLINE", false, a.Integration, a.now())
			if a.Instance != "" {
				offline.Provenance.SourceDescription = Lattice.String(a.instanceTag())
			}
			_, _ = a.Entities.PublishEntity(off, offline)
			cancel()
			return
		case m := <-a.Inbox:
			a.handle(ctx, m)
		case <-tick.C:
			a.Step(ctx, time.Second.Seconds())
		}
	}
}

func (a *Asset) handle(ctx context.Context, m inbox) {
	switch {
	case m.execute != nil:
		a.accept(ctx, m.execute)
	case m.cancel != "" && a.task != nil && a.task.id == m.cancel:
		a.Log.Info("task cancelled by operator", "task", m.cancel)
		a.finish(ctx, Lattice.TaskStatusStatusStatusDoneNotOk, errCode(Lattice.TaskErrorCodeErrorCodeCancelled, "cancelled"))
	case m.complete != "" && a.task != nil && a.task.id == m.complete:
		a.Log.Info("task completed by operator", "task", m.complete)
		a.finish(ctx, Lattice.TaskStatusStatusStatusDoneOk, nil)
	}
}

// accept runs ACK → WILCO → EXECUTING, or rejects the task.
func (a *Asset) accept(ctx context.Context, t *Lattice.Task) {
	id := deref(t.Version.TaskID)
	if a.task != nil && a.task.id == id {
		return // duplicate delivery (e.g. after a reconnect)
	}
	at := &activeTask{id: id, started: a.now()}
	if t.Version.StatusVersion != nil {
		at.version = *t.Version.StatusVersion
	}
	current := ""
	if t.Status != nil && t.Status.Status != nil {
		current = string(*t.Status.Status)
	}
	if a.task != nil && !a.task.terminated {
		a.reject(ctx, at, "asset busy with task "+a.task.id)
		return
	}
	kind, targetID, point, err := parseObjective(t.Specification)
	if err != nil {
		a.reject(ctx, at, err.Error())
		return
	}
	at.kind, at.targetID = kind, targetID
	a.task = at
	resumed := current == string(Lattice.TaskStatusStatusStatusExecuting)
	if !resumed {
		a.setStatus(ctx, Lattice.TaskStatusStatusStatusAck, nil)
	}
	if point != nil {
		at.fix, at.fixAt, at.lastSeen = *point, a.now(), a.now()
	} else if !a.refreshTarget(ctx, true) {
		a.finish(ctx, Lattice.TaskStatusStatusStatusDoneNotOk, errCode(Lattice.TaskErrorCodeErrorCodeInvalid, "target entity "+targetID+" not found"))
		return
	}
	// Feasibility: there and back within endurance.
	need := 2 * geo.Haversine(a.state.Pos, at.fix) / a.Cfg.CruiseMps
	if time.Duration(need*float64(time.Second)) > a.Cfg.Endurance {
		a.finish(ctx, Lattice.TaskStatusStatusStatusDoneNotOk, errCode(Lattice.TaskErrorCodeErrorCodeRejected, "target beyond endurance"))
		return
	}
	if !resumed {
		a.setStatus(ctx, Lattice.TaskStatusStatusStatusWilco, nil)
		a.setStatus(ctx, Lattice.TaskStatusStatusStatusExecuting, nil)
	}
	a.mode = ModeTransit
	a.Log.Info("task accepted", "task", id, "kind", kind, "target", nonEmpty(targetID, "point"),
		"distance_km", fmt.Sprintf("%.1f", geo.Haversine(a.state.Pos, at.fix)/1000), "resumed", resumed)
}

func (a *Asset) reject(ctx context.Context, at *activeTask, why string) {
	a.Log.Warn("task rejected", "task", at.id, "reason", why)
	prev, prevMode := a.task, a.mode
	a.task = at
	a.setStatus(ctx, Lattice.TaskStatusStatusStatusDoneNotOk, errCode(Lattice.TaskErrorCodeErrorCodeRejected, why))
	at.terminated = true
	if prev != nil {
		a.task, a.mode = prev, prevMode // keep executing the current task
	}
}

// Step advances the simulation by dt seconds.
func (a *Asset) Step(ctx context.Context, dt float64) {
	now := a.now()
	speed := a.Cfg.CruiseMps
	if t := a.task; t != nil && !t.terminated {
		if t.targetID != "" && now.Sub(t.lastFetch) >= a.Behaviour.TargetRefresh {
			a.refreshTarget(ctx, false)
		}
		switch {
		case now.Sub(t.lastSeen) > a.Behaviour.TargetLostAfter:
			a.finish(ctx, Lattice.TaskStatusStatusStatusDoneNotOk, errCode(Lattice.TaskErrorCodeErrorCodeFailed, "target lost"))
		case now.Sub(t.started) > a.Behaviour.MaxTaskDuration:
			a.finish(ctx, Lattice.TaskStatusStatusStatusDoneNotOk, errCode(Lattice.TaskErrorCodeErrorCodeTimeout, "task exceeded maximum duration"))
		default:
			target := a.predict(now)
			dist := geo.Haversine(a.state.Pos, target)
			targetSpeed := hypot(t.vel)
			if dist <= a.sensorRange() {
				t.inRange += time.Duration(dt * float64(time.Second))
				a.mode = ModeOnStation
				if targetSpeed > 0.8*speed {
					a.state = StepToward(a.state, target, speed, dt) // cannot orbit a faster target: trail it
				} else {
					a.state = OrbitStep(a.state, target, a.Behaviour.OrbitRadius, speed, dt)
				}
			} else {
				a.mode = ModeTransit
				a.state = StepToward(a.state, target, speed, dt)
			}
			a.status = fmt.Sprintf("%s %s → %s · %.1f km · on target %s/%s", a.mode, t.kind, nonEmpty(t.targetID, "search area"),
				dist/1000, t.inRange.Round(time.Second), a.dwell(t.kind))
			if t.inRange >= a.dwell(t.kind) {
				a.Log.Info("mission complete", "task", t.id, "on_target", t.inRange.Round(time.Second))
				a.finish(ctx, Lattice.TaskStatusStatusStatusDoneOk, nil)
			}
		}
	}
	switch a.mode {
	case ModeRTB:
		a.state = StepToward(a.state, a.Cfg.Home, speed, dt)
		a.status = fmt.Sprintf("RTB · %.1f km to home", geo.Haversine(a.state.Pos, a.Cfg.Home)/1000)
		if geo.Haversine(a.state.Pos, a.Cfg.Home) < 50 {
			a.mode, a.state.SpeedMps, a.status = ModeIdle, 0, "IDLE at home"
			a.Log.Info("landed")
		}
	case ModeIdle:
		a.state.SpeedMps = 0
	}
	// Single-writer check before publishing: a foreign instance publishing on
	// top of us means two simulators drive this asset.
	if a.Instance != "" && now.Sub(a.lastCheck) >= 10*time.Second {
		a.lastCheck = now
		if err := a.CheckExclusive(ctx); err != nil {
			a.Log.Error("single-writer violation, stopping", "err", err)
			if a.OnConflict != nil {
				a.OnConflict(err)
			}
			return
		}
	}
	if a.mode != ModeIdle || now.Sub(a.lastPublish) >= 5*time.Second {
		a.publish(ctx, true)
	}
}

func (a *Asset) sensorRange() float64 {
	if a.Cfg.Domain == domain.DomainSea {
		return a.Behaviour.SensorRangeSea
	}
	return a.Behaviour.SensorRangeAir
}

func (a *Asset) dwell(kind string) time.Duration {
	switch kind {
	case "SHADOW":
		return a.Behaviour.DwellShadow
	case "RF":
		return a.Behaviour.DwellRF
	}
	return a.Behaviour.DwellVID
}

// predict extrapolates the last target fix with its published velocity.
func (a *Asset) predict(now time.Time) geo.Point {
	t := a.task
	dt := now.Sub(t.fixAt).Seconds()
	if dt <= 0 || hypot(t.vel) == 0 {
		return t.fix
	}
	return geo.Destination(t.fix, geo.NormalizeDeg(atan2deg(t.vel[0], t.vel[1])), hypot(t.vel)*dt)
}

// refreshTarget reads the target entity. It returns false when unavailable.
func (a *Asset) refreshTarget(ctx context.Context, first bool) bool {
	t := a.task
	t.lastFetch = a.now()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e, err := a.Entities.GetEntity(cctx, &Lattice.GetEntityRequest{EntityID: t.targetID})
	if err != nil || e.Location == nil || e.Location.Position == nil || e.Location.Position.LatitudeDegrees == nil {
		if !first {
			a.Log.Debug("target not visible", "target", t.targetID, "err", lattice.Describe(err))
		}
		return false
	}
	p := e.Location.Position
	t.fix = geo.Point{Lat: *p.LatitudeDegrees, Lon: deref(p.LongitudeDegrees)}
	t.fixAt, t.lastSeen = a.now(), a.now()
	t.vel = [2]float64{}
	if v := e.Location.VelocityEnu; v != nil {
		t.vel = [2]float64{deref(v.E), deref(v.N)}
	}
	return true
}

// finish moves the task to a terminal state (sent once) and returns home.
func (a *Asset) finish(ctx context.Context, st Lattice.TaskStatusStatus, terr *Lattice.TaskError) {
	t := a.task
	if t == nil || t.terminated {
		return
	}
	a.setStatus(ctx, st, terr)
	t.terminated = true
	a.mode = ModeRTB
	a.Log.Info("task finished", "task", t.id, "status", st, "error", errMsg(terr))
}

// setStatus sends the next strictly increasing status version. If the server
// rejects the version it re-reads the task and retries once (SPEC §8.5).
func (a *Asset) setStatus(ctx context.Context, st Lattice.TaskStatusStatus, terr *Lattice.TaskError) {
	t := a.task
	send := func(v int) error {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		resp, err := a.Tasks.UpdateTaskStatus(cctx, &Lattice.TaskStatusUpdate{
			TaskID: t.id, StatusVersion: &v,
			NewStatus: &Lattice.TaskStatus{Status: &st, TaskError: terr},
			Author:    &Lattice.Principal{System: &Lattice.System{EntityID: Lattice.String(a.Cfg.ID)}},
		})
		if err == nil && resp != nil && resp.Status != nil && resp.Status.Status != nil &&
			*resp.Status.Status == Lattice.TaskStatusStatusStatusVersionRejected {
			return fmt.Errorf("status version %d rejected", v)
		}
		return err
	}
	v := t.version + 1
	err := send(v)
	if err != nil {
		if cur, gerr := a.Tasks.GetTask(ctx, &Lattice.GetTaskRequest{TaskID: t.id}); gerr == nil && cur.Version != nil && cur.Version.StatusVersion != nil {
			v = *cur.Version.StatusVersion + 1
			err = send(v)
		}
	}
	if err != nil {
		a.Log.Error("task status update failed", "task", t.id, "status", st, "err", lattice.Describe(err))
		return
	}
	t.version = v
	a.Log.Info("task status", "task", t.id, "status", st, "version", v)
}

func (a *Asset) publish(ctx context.Context, online bool) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := a.now()
	ent := entity(a.Cfg, a.state, a.mode, a.status, online, a.Integration, now)
	if a.Instance != "" {
		ent.Provenance.SourceDescription = Lattice.String(a.instanceTag())
	}
	if _, err := a.Entities.PublishEntity(cctx, ent); err != nil {
		a.Log.Warn("asset publish failed", "err", lattice.Describe(err))
		return
	}
	a.lastPublish = now
}

// parseObjective reads a VisualId / Investigate specification.
func parseObjective(spec *Lattice.GoogleProtobufAny) (kind, targetID string, point *geo.Point, err error) {
	if spec == nil || spec.Type == nil {
		return "", "", nil, fmt.Errorf("task has no specification")
	}
	obj, _ := spec.ExtraProperties["objective"].(map[string]any)
	if obj == nil {
		return "", "", nil, fmt.Errorf("specification has no objective")
	}
	if id, ok := obj["entityId"].(string); ok && id != "" {
		targetID = id
	} else if pt, ok := obj["point"].(map[string]any); ok {
		lla, _ := pt["lla"].(map[string]any)
		lat, ok1 := lla["lat"].(float64)
		lon, ok2 := lla["lon"].(float64)
		if !ok1 || !ok2 {
			return "", "", nil, fmt.Errorf("objective point has no lat/lon")
		}
		point = &geo.Point{Lat: lat, Lon: lon}
	} else {
		return "", "", nil, fmt.Errorf("objective has neither entityId nor point")
	}
	switch {
	case strings.HasSuffix(*spec.Type, ".VisualId"):
		kind = "VID"
	case strings.HasSuffix(*spec.Type, ".Investigate") && point != nil:
		kind = "RF"
	case strings.HasSuffix(*spec.Type, ".Investigate"):
		kind = "SHADOW"
	default:
		return "", "", nil, fmt.Errorf("unsupported task type %s", *spec.Type)
	}
	return kind, targetID, point, nil
}

func errCode(c Lattice.TaskErrorCode, msg string) *Lattice.TaskError {
	return &Lattice.TaskError{Code: &c, Message: Lattice.String(msg)}
}

func errMsg(e *Lattice.TaskError) string {
	if e == nil || e.Message == nil {
		return ""
	}
	return *e.Message
}

func deref[T any](p *T) T {
	var z T
	if p == nil {
		return z
	}
	return *p
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
