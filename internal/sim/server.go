// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package sim is the offline sandbox: an in-memory stand-in for the subset of
// the Lattice REST API that MDTI uses (entities with overrides and expiry,
// tasks with the agent SSE stream, objects), a stub of the OpenAI Decisions
// API that replays recorded answers, and a local map of the picture.
//
// It exists so that anyone can run the full pipeline without Lattice or
// OpenAI credentials. The binaries are unchanged: they talk to it through the
// official SDKs, only LATTICE_ENDPOINT and OPENAI_BASE_URL point elsewhere.
// It is NOT Lattice: behaviour was modelled on what the live Sandbox did
// during development, and only the endpoints MDTI calls exist.
package sim

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
)

// Server is the in-memory sandbox. The zero value is not usable; call New.
type Server struct {
	Log *slog.Logger
	Now func() time.Time

	mu        sync.Mutex
	entities  map[string]*Lattice.Entity
	overrides map[string]Lattice.MilViewDisposition
	tasks     map[string]*Lattice.Task
	objects   map[string]object
	agents    map[*agentConn]struct{}
	events    []Event
	seq       int
}

type object struct {
	data []byte
	at   time.Time
}

// Event is one line of the sandbox activity log shown on the map.
type Event struct {
	Seq  int       `json:"seq"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // override | task | decision | object
	Text string    `json:"text"`
	// Entity is the entity the event concerns (map focus), if any.
	Entity string `json:"entity,omitempty"`
}

// overridable lists the field paths the Sandbox accepted overrides on.
var overridable = []string{"mil_view.disposition"}

// New returns an empty sandbox.
func New(log *slog.Logger) *Server {
	return &Server{
		Log: log, Now: time.Now,
		entities: map[string]*Lattice.Entity{}, overrides: map[string]Lattice.MilViewDisposition{},
		tasks: map[string]*Lattice.Task{}, objects: map[string]object{}, agents: map[*agentConn]struct{}{},
	}
}

// Handler serves the Lattice subset, the Decisions stub and the map.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("PUT /api/v1/entities", s.publishEntity)
	m.HandleFunc("GET /api/v1/entities/{id}", s.getEntity)
	m.HandleFunc("PUT /api/v1/entities/{id}/override/{field}", s.overrideEntity)
	m.HandleFunc("DELETE /api/v1/entities/{id}/override/{field}", s.removeOverride)
	m.HandleFunc("POST /api/v1/tasks", s.createTask)
	m.HandleFunc("GET /api/v1/tasks/{id}", s.getTask)
	m.HandleFunc("PUT /api/v1/tasks/{id}/status", s.updateStatus)
	m.HandleFunc("PUT /api/v1/tasks/{id}/cancel", s.cancelTask)
	m.HandleFunc("POST /api/v1/tasks/query", s.queryTasks)
	m.HandleFunc("POST /api/v1/agent/stream", s.agentStream)
	m.HandleFunc("POST /api/v1/objects/{path...}", s.uploadObject)
	m.HandleFunc("GET /api/v1/objects/{path...}", s.getObject)
	m.HandleFunc("HEAD /api/v1/objects/{path...}", s.getObject)
	m.HandleFunc("DELETE /api/v1/objects/{path...}", s.deleteObject)
	m.HandleFunc("POST /v1/decisions", s.decide)
	m.HandleFunc("GET /sim/state", s.state)
	m.HandleFunc("GET /{$}", s.mapPage)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotImplemented, "%s %s is not part of the offline sandbox", r.Method, r.URL.Path)
	})
	return m
}

// ---------------------------------------------------------------------------
// helpers

func fail(w http.ResponseWriter, code int, format string, a ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf(format, a...)})
}

func reply(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fail(w, http.StatusInternalServerError, "encode: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// clone deep-copies an SDK value through its JSON form, so stored state is
// never shared with a response or a stream.
func clone[T any](v *T) *T {
	b, _ := json.Marshal(v)
	out := new(T)
	_ = json.Unmarshal(b, out)
	return out
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// logEvent appends to the activity log (caller holds s.mu).
func (s *Server) logEvent(kind, entity, format string, a ...any) {
	s.seq++
	ev := Event{Seq: s.seq, At: s.Now().UTC(), Kind: kind, Entity: entity, Text: fmt.Sprintf(format, a...)}
	s.events = append(s.events, ev)
	if len(s.events) > 200 {
		s.events = slices.Clone(s.events[len(s.events)-200:])
	}
	s.Log.Info("sandbox", "kind", kind, "entity", entity, "text", ev.Text)
}

// ---------------------------------------------------------------------------
// entities

func (s *Server) publishEntity(w http.ResponseWriter, r *http.Request) {
	var e Lattice.Entity
	if err := decode(r, &e); err != nil {
		fail(w, http.StatusBadRequest, "invalid entity: %v", err)
		return
	}
	// The Sandbox rejected entities missing these (SPEC §5.2).
	switch {
	case deref(e.EntityID) == "":
		fail(w, http.StatusBadRequest, "entityId is required")
		return
	case e.ExpiryTime == nil:
		fail(w, http.StatusBadRequest, "expiryTime is required")
		return
	case e.Provenance == nil || deref(e.Provenance.IntegrationName) == "" || deref(e.Provenance.DataType) == "" ||
		e.Provenance.SourceUpdateTime == nil:
		fail(w, http.StatusBadRequest, "provenance.integrationName, dataType and sourceUpdateTime are required")
		return
	}
	s.mu.Lock()
	s.entities[*e.EntityID] = clone(&e)
	out := s.view(*e.EntityID)
	s.mu.Unlock()
	reply(w, out)
}

// view returns the entity as readers see it: overrides applied, expired
// entities gone (caller holds s.mu).
func (s *Server) view(id string) *Lattice.Entity {
	e, ok := s.entities[id]
	if !ok {
		return nil
	}
	if e.ExpiryTime != nil && s.Now().After(*e.ExpiryTime) {
		delete(s.entities, id)
		return nil
	}
	out := clone(e)
	if d, ok := s.overrides[id]; ok {
		if out.MilView == nil {
			out.MilView = &Lattice.MilView{}
		}
		out.MilView.Disposition = &d
	}
	// The Sandbox omitted DISPOSITION_UNKNOWN on read.
	if out.MilView != nil && out.MilView.Disposition != nil && *out.MilView.Disposition == Lattice.MilViewDispositionDispositionUnknown {
		out.MilView.Disposition = nil
	}
	return out
}

func (s *Server) getEntity(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	e := s.view(r.PathValue("id"))
	s.mu.Unlock()
	if e == nil {
		fail(w, http.StatusNotFound, "entity %s not found", r.PathValue("id"))
		return
	}
	reply(w, e)
}

func (s *Server) overrideEntity(w http.ResponseWriter, r *http.Request) {
	id, field := r.PathValue("id"), r.PathValue("field")
	if !slices.Contains(overridable, field) {
		fail(w, http.StatusBadRequest, "field path %q is not overridable", field)
		return
	}
	var req struct {
		Entity *Lattice.Entity `json:"entity"`
	}
	if err := decode(r, &req); err != nil || req.Entity == nil || req.Entity.MilView == nil || req.Entity.MilView.Disposition == nil {
		fail(w, http.StatusBadRequest, "override needs entity.milView.disposition")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.view(id) == nil {
		fail(w, http.StatusNotFound, "entity %s not found", id)
		return
	}
	d := *req.Entity.MilView.Disposition
	if prev, ok := s.overrides[id]; !ok || prev != d {
		s.logEvent("override", id, "%s → %s", name(s.entities[id]), strings.TrimPrefix(string(d), "DISPOSITION_"))
	}
	s.overrides[id] = d
	reply(w, s.view(id))
}

func (s *Server) removeOverride(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.overrides[id]; ok {
		delete(s.overrides, id)
		s.logEvent("override", id, "override removed from %s", name(s.entities[id]))
	}
	e := s.view(id)
	if e == nil {
		fail(w, http.StatusNotFound, "entity %s not found", id)
		return
	}
	reply(w, e)
}

func name(e *Lattice.Entity) string {
	if e != nil && e.Aliases != nil && deref(e.Aliases.Name) != "" {
		return *e.Aliases.Name
	}
	if e != nil {
		return deref(e.EntityID)
	}
	return "?"
}

// ---------------------------------------------------------------------------
// tasks

var terminal = []Lattice.TaskStatusStatus{
	Lattice.TaskStatusStatusStatusDoneOk, Lattice.TaskStatusStatusStatusDoneNotOk,
}

func status(t *Lattice.Task) Lattice.TaskStatusStatus {
	if t.Status == nil || t.Status.Status == nil {
		return ""
	}
	return *t.Status.Status
}

func assigneeID(t *Lattice.Task) string {
	if t.Relations == nil || t.Relations.Assignee == nil || t.Relations.Assignee.System == nil {
		return ""
	}
	return deref(t.Relations.Assignee.System.EntityID)
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var c Lattice.TaskCreation
	if err := decode(r, &c); err != nil {
		fail(w, http.StatusBadRequest, "invalid task: %v", err)
		return
	}
	if c.Specification == nil || deref(c.Specification.Type) == "" {
		fail(w, http.StatusBadRequest, "specification with @type is required")
		return
	}
	id := deref(c.TaskID)
	if id == "" {
		id = newID()
	}
	now := s.Now().UTC()
	sent := Lattice.TaskStatusStatusStatusSent
	t := &Lattice.Task{
		Version:     &Lattice.TaskVersion{TaskID: &id, DefinitionVersion: Lattice.Int(1), StatusVersion: Lattice.Int(1)},
		DisplayName: c.DisplayName, Description: c.Description, Specification: c.Specification,
		CreatedBy: c.Author, LastUpdatedBy: c.Author, CreateTime: &now, LastUpdateTime: &now,
		Status: &Lattice.TaskStatus{Status: &sent}, Relations: c.Relations, IsExecutedElsewhere: c.IsExecutedElsewhere,
	}
	s.mu.Lock()
	if _, dup := s.tasks[id]; dup {
		s.mu.Unlock()
		fail(w, http.StatusConflict, "task %s already exists", id)
		return
	}
	s.tasks[id] = t
	s.logEvent("task", targetOf(t), "task %s %q → %s: STATUS_SENT", id[:8], deref(t.DisplayName), assigneeID(t))
	if !deref(t.IsExecutedElsewhere) {
		s.deliver(assigneeID(t), &Lattice.AgentStreamEvent{ExecuteRequest: &Lattice.ExecuteRequest{Task: clone(t)}})
	}
	out := clone(t)
	s.mu.Unlock()
	reply(w, out)
}

// targetOf extracts objective.entityId from the task specification, if any.
func targetOf(t *Lattice.Task) string {
	if t.Specification == nil {
		return ""
	}
	b, _ := json.Marshal(t.Specification)
	var spec struct {
		Objective struct {
			EntityID string `json:"entityId"`
		} `json:"objective"`
	}
	_ = json.Unmarshal(b, &spec)
	return spec.Objective.EntityID
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	t, ok := s.tasks[r.PathValue("id")]
	var out *Lattice.Task
	if ok {
		out = clone(t)
	}
	s.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, "task %s not found", r.PathValue("id"))
		return
	}
	reply(w, out)
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	var u Lattice.TaskStatusUpdate
	if err := decode(r, &u); err != nil || u.NewStatus == nil || u.NewStatus.Status == nil || u.StatusVersion == nil {
		fail(w, http.StatusBadRequest, "statusVersion and newStatus.status are required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[r.PathValue("id")]
	if !ok {
		fail(w, http.StatusNotFound, "task %s not found", r.PathValue("id"))
		return
	}
	cur := deref(t.Version.StatusVersion)
	switch {
	case slices.Contains(terminal, status(t)):
		fail(w, http.StatusConflict, "task is in terminal state %s", status(t))
		return
	case *u.StatusVersion <= cur:
		// Status versions must strictly increase (SPEC §8.5).
		fail(w, http.StatusConflict, "status version %d rejected: current is %d", *u.StatusVersion, cur)
		return
	}
	now := s.Now().UTC()
	st := *u.NewStatus.Status
	next := clone(u.NewStatus)
	if prev := t.Status; prev != nil && prev.StartTime != nil {
		next.StartTime = prev.StartTime
	}
	if st == Lattice.TaskStatusStatusStatusExecuting && next.StartTime == nil {
		next.StartTime = &now
	}
	t.Status, t.LastUpdatedBy, t.LastUpdateTime = next, u.Author, &now
	t.Version.StatusVersion = u.StatusVersion
	detail := ""
	if next.TaskError != nil && deref(next.TaskError.Message) != "" {
		detail = " (" + *next.TaskError.Message + ")"
	}
	id := deref(t.Version.TaskID)
	s.logEvent("task", targetOf(t), "task %s %s: %s v%d%s", id[:min(8, len(id))], assigneeID(t), st, *u.StatusVersion, detail)
	reply(w, clone(t))
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	var c Lattice.TaskCancellation
	_ = decode(r, &c)
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[r.PathValue("id")]
	if !ok {
		fail(w, http.StatusNotFound, "task %s not found", r.PathValue("id"))
		return
	}
	if slices.Contains(terminal, status(t)) {
		fail(w, http.StatusBadRequest, "task is in terminal state %s", status(t))
		return
	}
	id := deref(t.Version.TaskID)
	s.logEvent("task", targetOf(t), "task %s cancel requested", id[:min(8, len(id))])
	if !s.deliver(assigneeID(t), &Lattice.AgentStreamEvent{CancelRequest: &Lattice.CancelRequest{
		TaskID: &id, Assignee: t.Relations.Assignee, Author: c.Author}}) {
		// Nobody to tell: the cancellation completes immediately.
		notOK := Lattice.TaskStatusStatusStatusDoneNotOk
		t.Status = &Lattice.TaskStatus{Status: &notOK}
		t.Version.StatusVersion = Lattice.Int(deref(t.Version.StatusVersion) + 1)
	}
	reply(w, clone(t))
}

func (s *Server) queryTasks(w http.ResponseWriter, r *http.Request) {
	var q Lattice.TaskQuery
	_ = decode(r, &q)
	want := ""
	if q.StatusFilter != nil && q.StatusFilter.Status != nil {
		want = string(*q.StatusFilter.Status)
	}
	s.mu.Lock()
	res := &Lattice.TaskQueryResults{}
	for _, t := range s.tasks {
		if want == "" || string(status(t)) == want {
			res.Tasks = append(res.Tasks, clone(t))
		}
	}
	s.mu.Unlock()
	sort.Slice(res.Tasks, func(i, j int) bool {
		return deref(res.Tasks[i].CreateTime).Before(deref(res.Tasks[j].CreateTime))
	})
	reply(w, res)
}

// ---------------------------------------------------------------------------
// agent stream (SSE)

type agentConn struct {
	ids map[string]bool
	ch  chan *Lattice.AgentStreamEvent
}

// deliver pushes a request to every listening agent that serves the
// assignee; it reports whether one was found (caller holds s.mu).
func (s *Server) deliver(assignee string, ev *Lattice.AgentStreamEvent) bool {
	found := false
	for c := range s.agents {
		if c.ids[assignee] {
			found = true
			select {
			case c.ch <- ev:
			default:
				s.Log.Warn("agent stream backlog full, dropping request", "assignee", assignee)
			}
		}
	}
	return found
}

func (s *Server) agentStream(w http.ResponseWriter, r *http.Request) {
	var req Lattice.AgentStreamRequest
	if err := decode(r, &req); err != nil || req.AgentSelector == nil || len(req.AgentSelector.EntityIDs) == 0 {
		fail(w, http.StatusBadRequest, "agentSelector.entityIds is required")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	c := &agentConn{ids: map[string]bool{}, ch: make(chan *Lattice.AgentStreamEvent, 64)}
	for _, id := range req.AgentSelector.EntityIDs {
		c.ids[id] = true
	}
	s.mu.Lock()
	s.agents[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.agents, c)
		s.mu.Unlock()
	}()
	s.Log.Info("agent connected", "agents", req.AgentSelector.EntityIDs)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	hb := 30 * time.Second
	if req.HeartbeatIntervalMs != nil && *req.HeartbeatIntervalMs > 0 {
		hb = time.Duration(*req.HeartbeatIntervalMs) * time.Millisecond
	}
	tick := time.NewTicker(hb)
	defer tick.Stop()
	send := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if !send(map[string]string{"event": "heartbeat", "timestamp": s.Now().UTC().Format(time.RFC3339Nano)}) {
				return
			}
		case ev := <-c.ch:
			if !send(Lattice.StreamAsAgentResponse{AgentRequest: ev}) {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// objects

func (s *Server) uploadObject(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil || p == "" {
		fail(w, http.StatusBadRequest, "invalid upload")
		return
	}
	now := s.Now().UTC()
	sum := sha256.Sum256(data)
	s.mu.Lock()
	s.objects[p] = object{data: data, at: now}
	s.logEvent("object", "", "stored %s (%d bytes)", p, len(data))
	s.mu.Unlock()
	reply(w, map[string]any{
		"content_identifier": map[string]string{"path": p, "checksum": hex.EncodeToString(sum[:])},
		"size_bytes":         len(data), "last_updated_at": now,
	})
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	o, ok := s.objects[r.PathValue("path")]
	s.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, "object %s not found", r.PathValue("path"))
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(o.data)))
	w.Header().Set("Last-Modified", o.at.Format(http.TimeFormat))
	if r.Method == http.MethodHead {
		return
	}
	http.ServeContent(w, r, "", o.at, bytes.NewReader(o.data))
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	_, ok := s.objects[r.PathValue("path")]
	delete(s.objects, r.PathValue("path"))
	s.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, "object %s not found", r.PathValue("path"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
