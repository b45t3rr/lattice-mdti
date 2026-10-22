// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package domain

import "time"

// Disposition is MDTI's allegiance vocabulary (mapped to Lattice in C2).
type Disposition string

const (
	DispositionFriendly Disposition = "FRIENDLY"
	DispositionNeutral  Disposition = "NEUTRAL"
	DispositionSuspect  Disposition = "SUSPECT"
	DispositionHostile  Disposition = "HOSTILE"
)

// ThreatCategory is the Tier-2 classification (SPEC §3.3).
type ThreatCategory string

const (
	ThreatGNSSSpoofing   ThreatCategory = "GNSS_SPOOFING"
	ThreatADSBGhost      ThreatCategory = "ADSB_GHOST_INJECTION"
	ThreatKinematicSpoof ThreatCategory = "KINEMATIC_SPOOF"
	ThreatAISDark        ThreatCategory = "AIS_DARK_ACTIVITY"
	ThreatIUUFishing     ThreatCategory = "IUU_FISHING_SUSPECTED"
	ThreatBenignArtifact ThreatCategory = "BENIGN_SENSOR_ARTIFACT"
	ThreatUnknown        ThreatCategory = "UNKNOWN"
)

// ThreatCategories lists every category in a stable order.
var ThreatCategories = []ThreatCategory{
	ThreatGNSSSpoofing, ThreatADSBGhost, ThreatKinematicSpoof, ThreatAISDark,
	ThreatIUUFishing, ThreatBenignArtifact, ThreatUnknown,
}

// RecommendedTask is the ISR action Tier 2 recommends.
type RecommendedTask string

const (
	TaskNone                 RecommendedTask = "NONE"
	TaskVisualIdentification RecommendedTask = "VISUAL_IDENTIFICATION"
	TaskInterceptAndShadow   RecommendedTask = "INTERCEPT_AND_SHADOW"
	TaskRFEmitterLocate      RecommendedTask = "RF_EMITTER_LOCATE"
)

// Coordinates is a target position.
type Coordinates struct {
	Lat     float64  `json:"lat"`
	Lon     float64  `json:"lon"`
	AltHaeM *float64 `json:"alt_hae_m"`
}

// TacticalDecision is Tier 2's output (SPEC §3.3). Every field has a single,
// auditable origin: the model answers only probabilities and closed choices;
// everything else is deterministic policy.
type TacticalDecision struct {
	AnomalyID         string          `json:"anomaly_id"`
	EntityID          string          `json:"entity_id"`
	AnomalyConfirmed  bool            `json:"anomaly_confirmed"`
	Disposition       Disposition     `json:"disposition"` // recommended ("" = no change)
	ConfidenceScore   float64         `json:"confidence_score"`
	ThreatCategory    ThreatCategory  `json:"threat_category"`
	TacticalReasoning string          `json:"tactical_reasoning"`
	RecommendedTask   RecommendedTask `json:"recommended_task"`
	TargetCoordinates *Coordinates    `json:"target_coordinates"`

	// Action is what C2 is authorised to do after guardrails and the autonomy
	// level, which can be less than the recommendation.
	Action Action        `json:"action"`
	Audit  DecisionAudit `json:"audit"`
}

// Action is the authorised outcome of a decision.
type Action struct {
	Disposition  Disposition `json:"disposition,omitempty"` // override to apply ("" = none)
	DispatchTask bool        `json:"dispatch_task"`
}

// DecisionAudit records how the decision was reached (evidence only).
type DecisionAudit struct {
	Model              string             `json:"model"`
	QuestionSetVersion string             `json:"question_set_version"`
	PolicyVersion      string             `json:"policy_version"`
	PConfirmed         float64            `json:"p_confirmed"`
	CategoryChoice     string             `json:"category_choice"`
	CategoryProbs      map[string]float64 `json:"category_probs"`
	CategoryConfidence float64            `json:"category_confidence"`
	ThreatLevelScore   float64            `json:"threat_level_score"`
	ThreatLevelProbs   map[string]float64 `json:"threat_level_probs"`
	GuardrailsApplied  []string           `json:"guardrails_applied"`
	Fallback           string             `json:"fallback,omitempty"`
	Cached             bool               `json:"cached,omitempty"`
	LatencyMs          int64              `json:"latency_ms"`
	InputTokens        int64              `json:"input_tokens"`
	DecidedAt          time.Time          `json:"decided_at"`
}
