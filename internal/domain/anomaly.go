// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package domain

import "time"

// AnomalyType is the Tier-1 detector that fired (SPEC §3.2).
type AnomalyType string

const (
	AnomalyKinematicImpossible AnomalyType = "KINEMATIC_IMPOSSIBLE"
	AnomalyBaroGNSSDivergence  AnomalyType = "BARO_GNSS_DIVERGENCE"
	AnomalyGhostTrack          AnomalyType = "GHOST_TRACK"
	AnomalyAISDarkEEZ          AnomalyType = "AIS_DARK_EEZ"
	AnomalyNavIntegrityDrop    AnomalyType = "NAV_INTEGRITY_DROP"
)

// AnomalyEvent is Tier 1's output (SPEC §3.2).
type AnomalyEvent struct {
	ID            string             `json:"id"`
	EntityID      string             `json:"entity_id"`
	EntityKey     string             `json:"entity_key"`
	Domain        Domain             `json:"domain"`
	Type          AnomalyType        `json:"type"`
	Severity      int                `json:"severity"` // 1 info · 2 low · 3 medium · 4 high
	Rule          string             `json:"rule"`     // e.g. "D1.R2 teleport"
	Metrics       map[string]float64 `json:"metrics"`
	Corroborating []AnomalyType      `json:"corroborating,omitempty"`
	// Escalate is true when the event passes the Tier-2 gate (SPEC §6.8).
	Escalate   bool       `json:"escalate"`
	Window     []Frame    `json:"window"` // last ≤ 20 frames, Source stripped
	Geo        GeoContext `json:"geo"`
	DetectedAt time.Time  `json:"detected_at"`
}

// GeoContext is the geographic situation of the track when the event fired.
type GeoContext struct {
	NearestAerodrome   string   `json:"nearest_aerodrome"`
	DistToAerodromeNM  float64  `json:"dist_to_aerodrome_nm"`
	SignedDistToEEZNM  *float64 `json:"signed_dist_to_eez_nm,omitempty"` // negative = inside a monitored zone
	InsideEEZ          *bool    `json:"inside_eez,omitempty"`
	Zone               *ZoneRef `json:"zone,omitempty"`
	CoverageEdgeDistNM *float64 `json:"coverage_edge_dist_nm,omitempty"`
	AISCoveragePrior   *float64 `json:"ais_coverage_prior,omitempty"`
}

// ZoneRef identifies a monitored maritime zone.
type ZoneRef struct {
	ID          string `json:"id"`
	LegalStatus string `json:"legal_status"`
}
