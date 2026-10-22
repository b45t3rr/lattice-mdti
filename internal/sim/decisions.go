// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package sim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// StubModel is the model name the Decisions stub reports, so decisions made
// offline are never mistaken for real model output in logs or evidence.
const StubModel = "offline-stub (recorded answers)"

// recorded holds one canned answer set per anomaly type. Ghost and GNSS are
// the gpt-6-luna answers recorded in the live 3-act run (2026-10-22); dark
// vessel and kinematic follow the calibration probes (P(dark) = 0.86).
// The stub does not reason: it only lets the deterministic Go policy, the
// guardrails and C2 run end to end without an API key.
type recorded struct {
	p          float64
	category   domain.ThreatCategory
	catConf    float64
	catP       map[domain.ThreatCategory]float64
	level      float64
	levelProbs [3]float64
}

var answers = map[domain.AnomalyType]recorded{
	domain.AnomalyGhostTrack: {p: 0.91, category: domain.ThreatADSBGhost, catConf: 0.67,
		catP:  map[domain.ThreatCategory]float64{domain.ThreatADSBGhost: 0.72, domain.ThreatGNSSSpoofing: 0.22},
		level: 1.04, levelProbs: [3]float64{0, 0.96, 0.04}},
	domain.AnomalyBaroGNSSDivergence: {p: 0.95, category: domain.ThreatGNSSSpoofing, catConf: 1.0,
		catP:  map[domain.ThreatCategory]float64{domain.ThreatGNSSSpoofing: 1.0},
		level: 1.27, levelProbs: [3]float64{0.03, 0.67, 0.30}},
	domain.AnomalyAISDarkEEZ: {p: 0.86, category: domain.ThreatAISDark, catConf: 0.70,
		catP:  map[domain.ThreatCategory]float64{domain.ThreatAISDark: 0.70, domain.ThreatIUUFishing: 0.25},
		level: 1.05, levelProbs: [3]float64{0.02, 0.91, 0.07}},
	domain.AnomalyKinematicImpossible: {p: 0.93, category: domain.ThreatKinematicSpoof, catConf: 0.85,
		catP:  map[domain.ThreatCategory]float64{domain.ThreatKinematicSpoof: 0.85, domain.ThreatADSBGhost: 0.10},
		level: 1.10, levelProbs: [3]float64{0.02, 0.86, 0.12}},
}

// benign is the answer for anything else: not confirmed.
var benign = recorded{p: 0.20, category: domain.ThreatBenignArtifact, catConf: 0.80,
	catP:  map[domain.ThreatCategory]float64{domain.ThreatBenignArtifact: 0.80, domain.ThreatUnknown: 0.15},
	level: 0.15, levelProbs: [3]float64{0.86, 0.13, 0.01}}

var typeRe = regexp.MustCompile(`"type"\s*:\s*"([A-Z_]+)"`)

// anomalyType finds the anomaly type in the event JSON the engine sends.
func anomalyType(texts []string) domain.AnomalyType {
	for _, t := range texts {
		for _, m := range typeRe.FindAllStringSubmatch(t, -1) {
			if _, ok := answers[domain.AnomalyType(m[1])]; ok {
				return domain.AnomalyType(m[1])
			}
			if domain.AnomalyType(m[1]) == domain.AnomalyNavIntegrityDrop {
				return domain.AnomalyNavIntegrityDrop
			}
		}
	}
	return ""
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	var texts []string
	for _, in := range req.Input {
		for _, c := range in.Content {
			texts = append(texts, c.Text)
		}
	}
	typ := anomalyType(texts)
	a, ok := answers[typ]
	if !ok {
		a = benign
	}
	var probs []map[string]any
	for _, c := range domain.ThreatCategories {
		probs = append(probs, map[string]any{"value": string(c), "probability": a.catP[c]})
	}
	resp := map[string]any{
		"model": StubModel,
		"usage": map[string]any{"input_tokens": 0, "input_tokens_details": map[string]any{"cache_write_tokens": 0, "cached_tokens": 0},
			"output_tokens": 0, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 0},
		"answers": []map[string]any{
			{"type": "predicate", "name": "anomaly_confirmed", "probability": a.p},
			{"type": "choice", "name": "threat_category", "choice": string(a.category), "confidence": a.catConf, "probabilities": probs},
			{"type": "score", "name": "threat_level", "score": a.level, "confidence": 0.6, "probabilities": []map[string]any{
				{"value": 0, "label": "Benign", "probability": a.levelProbs[0]},
				{"value": 1, "label": "Suspicious", "probability": a.levelProbs[1]},
				{"value": 2, "label": "Hostile intent likely", "probability": a.levelProbs[2]},
			}},
		},
	}
	s.mu.Lock()
	s.logEvent("decision", "", "Tier-2 stub: %s → P=%.2f %s (recorded answer)", nonEmpty(string(typ), "unknown"), a.p, a.category)
	s.mu.Unlock()
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, "%s", b)
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
