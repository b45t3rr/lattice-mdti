// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package evidence stores each Tier-2 decision, with everything needed to
// audit it, in the Lattice Objects API (SPEC §7.10).
package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/option"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/tier2"
)

// ObjectsAPI is the subset of the Lattice objects client used here.
type ObjectsAPI interface {
	UploadObject(ctx context.Context, objectPath string, request io.Reader, opts ...option.RequestOption) (*Lattice.PathMetadata, error)
}

// Record is the evidence document.
type Record struct {
	SchemaVersion    int                     `json:"schema_version"`
	AnomalyEvent     domain.AnomalyEvent     `json:"anomaly_event"`
	DecisionEvidence json.RawMessage         `json:"decision_request_evidence,omitempty"` // exactly what the model saw
	DecisionAnswers  json.RawMessage         `json:"decision_answers_raw,omitempty"`
	TacticalDecision domain.TacticalDecision `json:"tactical_decision"`
}

// Path returns the object path for a decision.
func Path(entityID, anomalyID string) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, s)
	}
	return fmt.Sprintf("mdti/evidence/%s/%s.json", clean(entityID), clean(anomalyID))
}

// Build renders the evidence document for a result.
func Build(r tier2.Result) ([]byte, error) {
	rec := Record{SchemaVersion: 1, AnomalyEvent: r.Event, TacticalDecision: r.Decision}
	if len(r.Evidence) > 0 {
		rec.DecisionEvidence = r.Evidence
	}
	if r.RawAnswers != "" && json.Valid([]byte(r.RawAnswers)) {
		rec.DecisionAnswers = json.RawMessage(r.RawAnswers)
	}
	return json.MarshalIndent(rec, "", "  ")
}

// Upload stores the evidence and returns its object path.
func Upload(ctx context.Context, api ObjectsAPI, r tier2.Result) (string, error) {
	body, err := Build(r)
	if err != nil {
		return "", err
	}
	path := Path(r.Decision.EntityID, r.Decision.AnomalyID)
	if _, err := api.UploadObject(ctx, path, bytes.NewReader(body)); err != nil {
		return "", fmt.Errorf("upload %s: %w", path, err)
	}
	return path, nil
}
