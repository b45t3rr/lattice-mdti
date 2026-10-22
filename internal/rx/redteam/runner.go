// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package redteam

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Plan is a fully rendered attack: frames on an absolute timeline plus the
// expected anomalies.
type Plan struct {
	Frames []domain.Frame
	Truth  []Truth
}

// Render builds the attack deterministically from the scenario seed. epoch is
// the absolute time of the attack start (StartAfter is added). If target is
// nil the generator synthesizes one.
func (s *Scenario) Render(epoch time.Time, target *Track) (*Plan, error) {
	g, err := s.generator()
	if err != nil {
		return nil, err
	}
	t := g.synthTarget()
	if target != nil {
		t = *target
		if h, ok := g.(interface{ hijacked() }); ok {
			h.hijacked()
		}
	}
	rng := rand.New(rand.NewPCG(uint64(s.Seed), 0x6d647469)) // "mdti"
	timed, truth := g.build(s.Name, t, rng)
	sort.SliceStable(timed, func(i, j int) bool { return timed[i].At < timed[j].At })

	start := epoch.Add(s.StartAfter)
	plan := &Plan{Frames: make([]domain.Frame, 0, len(timed))}
	for _, tf := range timed {
		f := tf.Frame
		f.Source = domain.SourceRedTeam
		f.ObservedAt = start.Add(tf.At)
		f.ReceivedAt = f.ObservedAt
		plan.Frames = append(plan.Frames, f)
	}
	for _, tr := range truth {
		tr.TStart, tr.TEnd = start.Add(tr.Start), start.Add(tr.End)
		plan.Truth = append(plan.Truth, tr)
	}
	return plan, nil
}

// WriteTruth appends ground-truth records as JSONL.
func WriteTruth(w io.Writer, truth []Truth) error {
	enc := json.NewEncoder(w)
	for _, t := range truth {
		if err := enc.Encode(t); err != nil {
			return err
		}
	}
	return nil
}

// Runner injects a scenario into the live pipeline in real time.
type Runner struct {
	Scenario *Scenario
	Log      *slog.Logger
	// Truth receives the ground truth once the plan is rendered (optional).
	Truth io.Writer
}

// MaxHijackAge is the oldest live report a hijack may start from.
const MaxHijackAge = 5 * time.Second

// HijackFilter selects a live target worth hijacking: airborne, with an
// altitude and speed, at cruise-like altitude.
func HijackFilter(f domain.Frame) bool {
	return f.Domain == domain.DomainAir && f.OnGround != nil && !*f.OnGround &&
		f.AltBaroFt != nil && *f.AltBaroFt > 10_000 && f.GroundSpeedMps != nil && f.TrackDeg != nil && !f.IsMLAT
}

// Run waits StartAfter, optionally picks a live hijack target from live, then
// emits the plan on the wall clock. live may be nil.
func (r *Runner) Run(ctx context.Context, out chan<- domain.Frame, live <-chan domain.Frame) error {
	s := r.Scenario
	log := r.Log.With("module", "redteam", "scenario", s.Name, "attack", string(s.Attack))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.StartAfter):
	}

	var target *Track
	if s.Target.Mode == TargetHijack && live != nil {
		deadline := time.After(s.Target.HijackWait)
	pick:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline:
				log.Warn("no live hijack target found, synthesizing", "waited", s.Target.HijackWait)
				break pick
			case f, ok := <-live:
				if !ok {
					break pick
				}
				// The subscription buffers frames from start-up; extrapolating from a
				// stale report would itself look like a teleport.
				if HijackFilter(f) && time.Since(f.ObservedAt) < MaxHijackAge {
					t := TrackFromFrame(f)
					step(&t, time.Since(f.ObservedAt)) // bring the report up to now
					target = &t
					log.Info("hijacking live track", "entity_key", f.EntityKey)
					break pick
				}
			}
		}
	}

	epoch := time.Now()
	plan, err := s.Render(epoch, target)
	if err != nil {
		return err
	}
	// Render adds StartAfter, which has already elapsed here.
	shift := -s.StartAfter
	if r.Truth != nil {
		truth := make([]Truth, len(plan.Truth))
		for i, t := range plan.Truth {
			t.TStart, t.TEnd = t.TStart.Add(shift), t.TEnd.Add(shift)
			truth[i] = t
		}
		if err := WriteTruth(r.Truth, truth); err != nil {
			return err
		}
	}
	log.Info("attack started", "frames", len(plan.Frames))
	for _, f := range plan.Frames {
		f.ObservedAt = f.ObservedAt.Add(shift)
		if d := time.Until(f.ObservedAt); d > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		f.ReceivedAt = time.Now()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- f:
		}
	}
	log.Info("attack finished")
	return nil
}
