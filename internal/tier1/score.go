// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package tier1

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Truth is one ground-truth record written by the red-team injector.
type Truth struct {
	Scenario    string    `json:"scenario"`
	EntityKey   string    `json:"entity_key"`
	AnomalyType string    `json:"anomaly_type"`
	TStart      time.Time `json:"t_start"`
	TEnd        time.Time `json:"t_end"`
}

// ReadTruth loads groundtruth.jsonl.
func ReadTruth(r io.Reader) ([]Truth, error) {
	var out []Truth
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var t Truth
		if err := json.Unmarshal(sc.Bytes(), &t); err != nil {
			return nil, fmt.Errorf("truth line %d: %w", line, err)
		}
		out = append(out, t)
	}
	return out, sc.Err()
}

// Matching tolerances: a detection may lead the labelled window slightly
// (timestamp jitter) and lag it by up to a minute.
const (
	matchLead = 5 * time.Second
	matchLag  = 60 * time.Second
)

// Match pairs a truth record with the first event that detected it.
type Match struct {
	Truth   Truth
	Event   *domain.AnomalyEvent
	Latency time.Duration
}

// Report summarises detection quality against ground truth.
type Report struct {
	Matches        []Match
	FalsePositives []domain.AnomalyEvent // events on labelled entities with no matching truth
	Unlabelled     []domain.AnomalyEvent // events on entities absent from the truth (live traffic)
	Recall         float64
}

func covers(ev domain.AnomalyEvent, typ string) bool {
	return string(ev.Type) == typ || slices.Contains(ev.Corroborating, domain.AnomalyType(typ))
}

// Score evaluates events against truth.
func Score(events []domain.AnomalyEvent, truth []Truth) Report {
	var r Report
	labelled := map[string]bool{}
	for _, t := range truth {
		labelled[t.EntityKey] = true
	}
	used := make([]bool, len(events))
	for _, t := range truth {
		m := Match{Truth: t}
		for i := range events {
			ev := events[i]
			if ev.EntityKey != t.EntityKey || !covers(ev, t.AnomalyType) {
				continue
			}
			if ev.DetectedAt.Before(t.TStart.Add(-matchLead)) || ev.DetectedAt.After(t.TEnd.Add(matchLag)) {
				continue
			}
			used[i] = true
			if m.Event == nil {
				m.Event = &events[i]
				m.Latency = ev.DetectedAt.Sub(t.TStart)
			}
		}
		r.Matches = append(r.Matches, m)
	}
	hit := 0
	for _, m := range r.Matches {
		if m.Event != nil {
			hit++
		}
	}
	if len(truth) > 0 {
		r.Recall = float64(hit) / float64(len(truth))
	}
	for i, ev := range events {
		switch {
		case used[i]:
		case labelled[ev.EntityKey]:
			r.FalsePositives = append(r.FalsePositives, ev)
		default:
			r.Unlabelled = append(r.Unlabelled, ev)
		}
	}
	return r
}
