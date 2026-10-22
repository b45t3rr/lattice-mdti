// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package agent

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/client"

	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

// Agent connects the simulated assets to the Lattice Tasks API as taskable
// agents: one SSE stream for all assets, routed by assignee (SPEC §8.4).
type Agent struct {
	Unary  *client.Client // publishes, reads and status updates
	Stream *client.Client // StreamAsAgent (no client timeout)
	Assets []*Asset
	Log    *slog.Logger

	Delivered sync.Map // task id → struct{} (for logs/tests)
}

// Run refuses to start if another live instance drives any asset, then
// publishes every asset and streams tasks until ctx ends, reconnecting with
// backoff. After each (re)connect it recovers tasks that were delivered while
// disconnected, because delivery is neither acknowledged nor replayed. A
// single-writer violation detected later stops the whole agent.
func (g *Agent) Run(ctx context.Context) error {
	for _, a := range g.Assets {
		if err := a.CheckExclusive(ctx); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	for _, a := range g.Assets {
		a.OnConflict = func(err error) { cancel(err) }
		wg.Go(func() { a.Run(ctx) })
	}
	// The agent entity must be live before tasks can be routed to it.
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
	}
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := g.session(ctx)
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		d := backoff + time.Duration(rand.Int64N(int64(backoff)))
		g.Log.Warn("task stream disconnected", "err", lattice.Describe(err), "retry_in", d.Round(time.Millisecond))
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
	wg.Wait()
	if cause := context.Cause(ctx); errors.Is(cause, ErrDuplicateAgent) {
		return cause
	}
	return nil
}

func (g *Agent) ids() []string {
	ids := make([]string, 0, len(g.Assets))
	for _, a := range g.Assets {
		ids = append(ids, a.Cfg.ID)
	}
	return ids
}

func (g *Agent) session(ctx context.Context) error {
	hb := 15_000
	stream, err := g.Stream.Tasks.StreamAsAgent(ctx, &Lattice.AgentStreamRequest{
		AgentSelector:       &Lattice.EntityIDsSelector{EntityIDs: g.ids()},
		HeartbeatIntervalMs: &hb,
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	g.Log.Info("listening for tasks", "agents", g.ids())
	go g.recover(ctx)
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if msg.AgentRequest == nil {
			continue // heartbeat
		}
		r := msg.AgentRequest
		switch {
		case r.ExecuteRequest != nil && r.ExecuteRequest.Task != nil:
			t := r.ExecuteRequest.Task
			g.Delivered.Store(deref(t.Version.TaskID), struct{}{})
			g.Log.Info("task received", "task", deref(t.Version.TaskID), "assignee", assignee(t.Relations), "name", deref(t.DisplayName))
			g.route(assignee(t.Relations), inbox{execute: t})
		case r.CancelRequest != nil:
			g.route(principalID(r.CancelRequest.Assignee), inbox{cancel: deref(r.CancelRequest.TaskID)})
		case r.CompleteRequest != nil:
			g.route("", inbox{complete: deref(r.CompleteRequest.TaskID)})
		}
	}
}

// route delivers to the named asset, or to every asset when id is empty.
func (g *Agent) route(id string, m inbox) {
	for _, a := range g.Assets {
		if id == "" || a.Cfg.ID == id {
			select {
			case a.Inbox <- m:
			default:
				g.Log.Warn("asset inbox full, dropping request", "asset", a.Cfg.Name)
			}
		}
	}
}

// recoverable statuses: delivered but not finished.
var recoverable = []Lattice.TaskQueryStatusFilterStatus{
	"STATUS_SENT", "STATUS_MACHINE_RECEIPT", "STATUS_ACK", "STATUS_WILCO", "STATUS_EXECUTING",
}

// recover queries open tasks assigned to our assets and re-delivers them.
func (g *Agent) recover(ctx context.Context) {
	ids := g.ids()
	for _, st := range recoverable {
		var page *string
		for range 5 {
			st := st
			res, err := g.Unary.Tasks.QueryTasks(ctx, &Lattice.TaskQuery{PageToken: page, StatusFilter: &Lattice.TaskQueryStatusFilter{Status: &st}})
			if err != nil {
				g.Log.Debug("task recovery query failed", "status", st, "err", lattice.Describe(err))
				break
			}
			for _, t := range res.Tasks {
				id := assignee(t.Relations)
				if !slices.Contains(ids, id) {
					continue
				}
				if _, seen := g.Delivered.LoadOrStore(deref(t.Version.TaskID), struct{}{}); seen {
					continue
				}
				g.Log.Info("recovered undelivered task", "task", deref(t.Version.TaskID), "status", st, "assignee", id)
				g.route(id, inbox{execute: t})
			}
			if res.NextPageToken == nil || *res.NextPageToken == "" {
				break
			}
			page = res.NextPageToken
		}
	}
}

func assignee(r *Lattice.Relations) string {
	if r == nil {
		return ""
	}
	return principalID(r.Assignee)
}

func principalID(p *Lattice.Principal) string {
	if p == nil || p.System == nil {
		return ""
	}
	return deref(p.System.EntityID)
}
