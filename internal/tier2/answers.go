// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package tier2

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/openai/openai-go/v3"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Answers are the validated model outputs for one anomaly.
type Answers struct {
	PConfirmed         float64
	Category           domain.ThreatCategory
	CategoryConfidence float64
	CategoryProbs      map[string]float64
	LevelScore         float64 // expected level, 0..2
	LevelProbs         map[string]float64
	InputTokens        int64
}

// ErrInvalidAnswers marks any refusal, missing answer or out-of-range value;
// the engine falls back to deterministic policy on it (SPEC §7.9).
var ErrInvalidAnswers = errors.New("invalid decision answers")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidAnswers, fmt.Sprintf(format, a...))
}

func finite01(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) && x >= 0 && x <= 1 }

// ParseAnswers validates a Decisions API response against question set q-v1.
func ParseAnswers(d *openai.Decision) (Answers, error) {
	var a Answers
	if d == nil {
		return a, invalid("empty response")
	}
	a.InputTokens = d.Usage.InputTokens
	seen := map[string]bool{}
	for _, ans := range d.Answers {
		if seen[ans.Name] {
			return a, invalid("duplicate answer %q", ans.Name)
		}
		seen[ans.Name] = true
		if ans.Type == "refusal" {
			return a, invalid("model refused %q", ans.Name)
		}
		switch ans.Name {
		case QConfirmed:
			if ans.Type != "predicate" {
				return a, invalid("%s: unexpected type %q", ans.Name, ans.Type)
			}
			p := ans.AsPredicate().Probability
			if !finite01(p) {
				return a, invalid("%s: probability %v", ans.Name, p)
			}
			a.PConfirmed = p
		case QCategory:
			if ans.Type != "choice" {
				return a, invalid("%s: unexpected type %q", ans.Name, ans.Type)
			}
			c := ans.AsChoice()
			cat := domain.ThreatCategory(c.Choice.OfString)
			if !slices.Contains(domain.ThreatCategories, cat) {
				return a, invalid("%s: unknown choice %q", ans.Name, c.Choice.OfString)
			}
			if !finite01(c.Confidence) {
				return a, invalid("%s: confidence %v", ans.Name, c.Confidence)
			}
			a.Category, a.CategoryConfidence = cat, c.Confidence
			a.CategoryProbs = map[string]float64{}
			for _, p := range c.Probabilities {
				if !finite01(p.Probability) {
					return a, invalid("%s: probability %v", ans.Name, p.Probability)
				}
				a.CategoryProbs[p.Value.OfString] = p.Probability
			}
		case QLevel:
			if ans.Type != "score" {
				return a, invalid("%s: unexpected type %q", ans.Name, ans.Type)
			}
			s := ans.AsScore()
			if math.IsNaN(s.Score) || s.Score < 0 || s.Score > float64(len(levels)-1) {
				return a, invalid("%s: score %v", ans.Name, s.Score)
			}
			a.LevelScore = s.Score
			a.LevelProbs = map[string]float64{}
			for _, p := range s.Probabilities {
				if !finite01(p.Probability) {
					return a, invalid("%s: probability %v", ans.Name, p.Probability)
				}
				a.LevelProbs[p.Label] = p.Probability
			}
		default:
			return a, invalid("unexpected answer %q", ans.Name)
		}
	}
	for _, q := range []string{QConfirmed, QCategory, QLevel} {
		if !seen[q] {
			return a, invalid("missing answer %q", q)
		}
	}
	return a, nil
}
