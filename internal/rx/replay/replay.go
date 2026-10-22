// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package replay records frames to JSONL and plays them back (SPEC §4.3).
package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Recorder writes one JSON frame per line.
type Recorder struct {
	w   *bufio.Writer
	enc *json.Encoder
}

func NewRecorder(w io.Writer) *Recorder {
	bw := bufio.NewWriter(w)
	return &Recorder{w: bw, enc: json.NewEncoder(bw)}
}

func (r *Recorder) Write(f domain.Frame) error { return r.enc.Encode(f) }
func (r *Recorder) Flush() error               { return r.w.Flush() }

// Read loads a whole JSONL recording.
func Read(rd io.Reader) ([]domain.Frame, error) {
	var out []domain.Frame
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var f domain.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, f)
	}
	return out, sc.Err()
}

// Player emits recorded frames in ObservedAt order.
//
// Speed 0 means "as fast as possible" and keeps the original timestamps
// (offline analysis). Speed > 0 paces emission in wall-clock time divided by
// Speed and, with Rebase, shifts timestamps so the first frame is "now";
// relative timing is preserved, so kinematics are unchanged.
type Player struct {
	Frames []domain.Frame
	Speed  float64
	Rebase bool
}

func (p *Player) Run(ctx context.Context, out chan<- domain.Frame) error {
	if len(p.Frames) == 0 {
		return nil
	}
	first := p.Frames[0].ObservedAt
	start := time.Now()
	for _, f := range p.Frames {
		rel := f.ObservedAt.Sub(first)
		if p.Speed > 0 {
			due := start.Add(time.Duration(float64(rel) / p.Speed))
			if d := time.Until(due); d > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(d):
				}
			}
			if p.Rebase {
				shift := start.Sub(first)
				f.ObservedAt = f.ObservedAt.Add(shift)
				f.ReceivedAt = f.ReceivedAt.Add(shift)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- f:
		}
	}
	return nil
}
