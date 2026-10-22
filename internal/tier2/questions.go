// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package tier2 is the AI tactical decision engine (SPEC §7). It asks the
// OpenAI Decisions API for calibrated probabilities and closed choices, then
// turns them into a decision with deterministic, versioned policy.
package tier2

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// QuestionSetVersion identifies the brief + questions below. Changing either
// invalidates calibrated thresholds (SPEC §7.4).
const QuestionSetVersion = "q-v1"

//go:embed brief/v1.txt
var briefV1 string

// Question names: answers are matched by name, never by position.
const (
	QConfirmed = "anomaly_confirmed"
	QCategory  = "threat_category"
	QLevel     = "threat_level"
)

var categoryDescriptions = map[domain.ThreatCategory]string{
	domain.ThreatGNSSSpoofing:   "The aircraft's GNSS-derived position or geometric altitude is being manipulated: geometric altitude diverges from barometric altitude and/or NACp/NIC collapse.",
	domain.ThreatADSBGhost:      "A track that does not correspond to a real aircraft: it appears mid-coverage, airborne, with no history, typically without callsign or accuracy data.",
	domain.ThreatKinematicSpoof: "Positions reported for a real aircraft's identity are physically impossible: teleport-like jumps, speeds or accelerations no aircraft can achieve, or positions alternating between two places.",
	domain.ThreatAISDark:        "A vessel deliberately stopped transmitting AIS in a sensitive area, as opposed to leaving receiver coverage.",
	domain.ThreatIUUFishing:     "A fishing vessel whose behaviour suggests illegal, unreported or unregulated fishing in or towards the EEZ.",
	domain.ThreatBenignArtifact: "The telemetry is explained by a known benign effect: MLAT jitter, timestamp noise, stale velocity, non-standard atmosphere, or an AIS coverage gap.",
	domain.ThreatUnknown:        "None of the other categories fits the evidence well.",
}

var levels = []struct{ label, description string }{
	{"Benign", "No action needed: the activity poses no threat to the protected area."},
	{"Suspicious", "Needs identification: unexplained activity that should be checked by an asset."},
	{"Hostile intent likely", "Active deception or evasion close to sensitive infrastructure or sovereign waters."},
}

func questions() []openai.DecisionNewParamsQuestionUnion {
	choices := make([]openai.DecisionNewParamsQuestionChoiceChoice, 0, len(domain.ThreatCategories))
	for _, c := range domain.ThreatCategories {
		choices = append(choices, openai.DecisionNewParamsQuestionChoiceChoice{
			Value:       openai.DecisionNewParamsQuestionChoiceChoiceValueUnion{OfString: openai.String(string(c))},
			Description: openai.String(categoryDescriptions[c]),
		})
	}
	lv := make([]openai.DecisionNewParamsQuestionScoreLevel, 0, len(levels))
	for _, l := range levels {
		lv = append(lv, openai.DecisionNewParamsQuestionScoreLevel{Label: l.label, Description: openai.String(l.description)})
	}
	return []openai.DecisionNewParamsQuestionUnion{
		{OfPredicate: &openai.DecisionNewParamsQuestionPredicate{
			Name: openai.String(QConfirmed),
			Instructions: "The telemetry shows a genuine manipulation or evasion of the surveillance system " +
				"(spoofing, injection, or deliberate transponder silence), not a benign sensor, coverage or weather artefact.",
		}},
		{OfChoice: &openai.DecisionNewParamsQuestionChoice{
			Name:         openai.String(QCategory),
			Instructions: "Which category best explains the observed telemetry?",
			Choices:      choices,
		}},
		{OfScore: &openai.DecisionNewParamsQuestionScore{
			Name:         openai.String(QLevel),
			Instructions: "How threatening is this track to the protected area, given its position, behaviour and proximity?",
			Levels:       lv,
		}},
	}
}

// BuildParams assembles the request: the static brief (cache breakpoint) plus
// the evidence JSON for this anomaly.
func BuildParams(model string, ev domain.AnomalyEvent, safetyID string) (openai.DecisionNewParams, []byte, error) {
	evidence, err := json.Marshal(BuildEvidence(ev))
	if err != nil {
		return openai.DecisionNewParams{}, nil, fmt.Errorf("marshal evidence: %w", err)
	}
	brief := openai.DecisionInputPartParamOfInputText(briefV1)
	brief.OfInputText.PromptCacheBreakpoint = openai.NewDecisionPromptCacheBreakpointParam()
	data := openai.DecisionInputPartParamOfInputText("Evidence JSON:\n" + string(evidence))
	p := openai.DecisionNewParams{
		Model: model,
		Input: openai.DecisionNewParamsInputUnion{OfDecisionInputMessageArray: []openai.DecisionInputMessageParam{{
			Role:    "user",
			Content: openai.DecisionInputMessageContentUnionParam{OfParts: []openai.DecisionInputPartUnionParam{brief, data}},
		}}},
		Questions:      questions(),
		PromptCacheKey: openai.String("mdti-tier2-" + QuestionSetVersion),
	}
	if safetyID != "" {
		p.SafetyIdentifier = openai.String(safetyID)
	}
	return p, evidence, nil
}

// Evidence is the only variable model input (SPEC §7.3). It never contains
// the frame source, ground truth or credentials.
type Evidence struct {
	Anomaly struct {
		Type          domain.AnomalyType   `json:"type"`
		Rule          string               `json:"rule"`
		Explanation   string               `json:"explanation"` // what the detector measured, in words
		Severity      int                  `json:"severity"`
		Metrics       map[string]float64   `json:"metrics"`
		Corroborating []domain.AnomalyType `json:"corroborating"`
	} `json:"anomaly"`
	Domain      domain.Domain   `json:"domain"`
	TrackWindow []evidenceFrame `json:"track_window"`
	Geo         struct {
		NearestAerodrome string   `json:"nearest_aerodrome"`
		DistNM           float64  `json:"dist_nm"`
		SignedEEZNM      *float64 `json:"signed_eez_nm"`
		InsideEEZ        *bool    `json:"inside_eez"`
		ZoneID           string   `json:"zone_id,omitempty"`
		ZoneLegalStatus  string   `json:"zone_legal_status,omitempty"`
		CoverageEdgeNM   *float64 `json:"adsb_coverage_edge_nm,omitempty"`
		AISCoveragePrior *float64 `json:"ais_coverage_prior,omitempty"`
	} `json:"geo"`
	// Identifiers are broadcast by the tracked object and may be chosen by an
	// adversary, so the model never sees their text: only structural features
	// (SPEC §7.7). This removes the prompt-injection channel entirely.
	Identifiers IdentifierFeatures `json:"identifier_features"`
}

type evidenceFrame struct {
	T         string   `json:"t"` // seconds relative to the detection, e.g. "-12.0"
	Lat       float64  `json:"lat"`
	Lon       float64  `json:"lon"`
	AltBaroFt *float64 `json:"alt_baro_ft,omitempty"`
	AltGeomFt *float64 `json:"alt_geom_ft,omitempty"`
	GSMps     *float64 `json:"gs_mps,omitempty"`
	TrackDeg  *float64 `json:"trk_deg,omitempty"`
	NIC       *int     `json:"nic,omitempty"`
	NACp      *int     `json:"nac_p,omitempty"`
	MLAT      bool     `json:"mlat,omitempty"`
	OnGround  *bool    `json:"on_ground,omitempty"`
	Squawk    string   `json:"squawk,omitempty"`
	Category  string   `json:"category,omitempty"`
	ShipType  *int     `json:"ship_type,omitempty"`
	NavStatus *int     `json:"nav_status,omitempty"`
	AISClass  string   `json:"ais_class,omitempty"`
}

// BuildEvidence converts an anomaly into the model's evidence document.
func BuildEvidence(ev domain.AnomalyEvent) Evidence {
	var e Evidence
	e.Anomaly.Type, e.Anomaly.Rule, e.Anomaly.Severity = ev.Type, ev.Rule, ev.Severity
	e.Anomaly.Explanation = Explain(ev)
	e.Anomaly.Metrics = roundMetrics(ev.Metrics)
	e.Anomaly.Corroborating = ev.Corroborating
	e.Domain = ev.Domain
	for _, f := range ev.Window {
		e.TrackWindow = append(e.TrackWindow, evidenceFrame{
			T:   fmt.Sprintf("%.1f", f.ObservedAt.Sub(ev.DetectedAt).Seconds()),
			Lat: round(f.Lat, 5), Lon: round(f.Lon, 5),
			AltBaroFt: f.AltBaroFt, AltGeomFt: f.AltGeomFt, GSMps: roundPtr(f.GroundSpeedMps, 1),
			TrackDeg: roundPtr(f.TrackDeg, 1), NIC: f.NIC, NACp: f.NACp, MLAT: f.IsMLAT, OnGround: f.OnGround,
			Squawk: f.Squawk, Category: f.Category, ShipType: f.ShipType, NavStatus: f.NavStatus, AISClass: f.AISClass,
		})
	}
	g := ev.Geo
	e.Geo.NearestAerodrome, e.Geo.DistNM = g.NearestAerodrome, round(g.DistToAerodromeNM, 1)
	e.Geo.SignedEEZNM, e.Geo.InsideEEZ = roundPtr(g.SignedDistToEEZNM, 1), g.InsideEEZ
	if g.Zone != nil {
		e.Geo.ZoneID, e.Geo.ZoneLegalStatus = g.Zone.ID, g.Zone.LegalStatus
	}
	e.Geo.CoverageEdgeNM, e.Geo.AISCoveragePrior = roundPtr(g.CoverageEdgeDistNM, 1), roundPtr(g.AISCoveragePrior, 2)
	if n := len(ev.Window); n > 0 {
		e.Identifiers = identifierFeatures(ev.Window[n-1])
	}
	return e
}

// IdentifierFeatures describes broadcast identifiers without their text.
type IdentifierFeatures struct {
	CallsignFormat      string `json:"callsign_format,omitempty"` // none | airline | registration | other
	RegistrationPresent bool   `json:"registration_present,omitempty"`
	AircraftType        string `json:"aircraft_type,omitempty"` // only if it is a valid ICAO type designator
	ShipNamePresent     bool   `json:"ship_name_present,omitempty"`
	IMOPresent          bool   `json:"imo_present,omitempty"`
}

var (
	airlineCallsign = regexp.MustCompile(`^[A-Z]{3}[0-9]{1,4}[A-Z]{0,2}$`)
	registrationID  = regexp.MustCompile(`^[A-Z0-9]{1,2}-?[A-Z0-9]{2,5}$`)
	icaoType        = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,3}$`)
)

func identifierFeatures(f domain.Frame) IdentifierFeatures {
	var x IdentifierFeatures
	if f.Domain == domain.DomainSea {
		x.ShipNamePresent, x.IMOPresent = f.ShipName != "", f.IMO != ""
		return x
	}
	switch c := strings.ToUpper(strings.TrimSpace(f.Callsign)); {
	case c == "":
		x.CallsignFormat = "none"
	case airlineCallsign.MatchString(c):
		x.CallsignFormat = "airline"
	case registrationID.MatchString(c):
		x.CallsignFormat = "registration"
	default:
		x.CallsignFormat = "other"
	}
	x.RegistrationPresent = f.Registration != ""
	if t := strings.ToUpper(f.AircraftType); icaoType.MatchString(t) {
		x.AircraftType = t
	}
	return x
}

func round(x float64, digits int) float64 {
	p := math.Pow10(digits)
	return math.Round(x*p) / p
}

func roundPtr(x *float64, digits int) *float64 {
	if x == nil {
		return nil
	}
	v := round(*x, digits)
	return &v
}

func roundMetrics(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = round(v, 3)
	}
	return out
}

// Explain renders what the Tier-1 detector measured as a factual sentence,
// built only from measured values (never from untrusted identifiers), so the
// model judges observable criteria rather than bare numbers.
func Explain(ev domain.AnomalyEvent) string {
	base := explainRule(ev)
	if ev.Domain == domain.DomainAir {
		base += positionQuality(ev.Window)
	}
	return base
}

// positionQuality states how the positions were derived and how accurate the
// aircraft claims them to be.
func positionQuality(win []domain.Frame) string {
	if len(win) == 0 {
		return ""
	}
	mlat := 0
	for _, f := range win {
		if f.IsMLAT {
			mlat++
		}
	}
	var s string
	if mlat*2 >= len(win) {
		s = " Positions are multilateration-derived (MLAT, computed by receivers rather than reported by the aircraft's GNSS)."
	} else {
		s = " Positions are reported by the aircraft's own GNSS (ADS-B)."
	}
	if last := win[len(win)-1]; last.NACp == nil {
		s += " The aircraft reports no navigation accuracy (NACp)."
	} else {
		s += fmt.Sprintf(" Latest NACp %d.", *last.NACp)
	}
	return s
}

func explainRule(ev domain.AnomalyEvent) string {
	m := ev.Metrics
	g := func(k string) float64 { return m[k] }
	switch {
	case ev.Rule == "D1.R2 teleport":
		return fmt.Sprintf("Consecutive position reports for the same transponder identity are %.1f NM apart %.1f s apart, "+
			"an implied ground speed of %.0f m/s against a plausible maximum of %.0f m/s for this aircraft category.",
			g("jump_m")/domain.NMToM, g("dt_s"), g("implied_speed_mps"), g("vmax_mps"))
	case ev.Rule == "D1.R5 oscillation":
		return fmt.Sprintf("Positions for the same transponder identity jump back and forth between two places (%.0f jumps, %.0f direction reversals within 60 s), "+
			"as when a second transmitter re-broadcasts a real aircraft's identity.", g("jumps_in_window"), g("reversals"))
	case ev.Rule == "D1.R1 overspeed":
		return fmt.Sprintf("Position-derived ground speed over at least 8 s is %.0f m/s, above the %.0f m/s maximum for this aircraft category, sustained.",
			g("implied_speed_mps"), g("vmax_mps"))
	case ev.Rule == "D1.R3 speed residual":
		return fmt.Sprintf("Position-derived speed (%.0f m/s) disagrees with the aircraft's own reported ground speed (%.0f m/s) by %.0f m/s over %.0f s.",
			g("implied_speed_mps"), g("reported_speed_mps"), g("residual_mps"), g("span_s"))
	case ev.Rule == "D1.R4 acceleration":
		return fmt.Sprintf("Reported ground speed changes at %.1f m/s² over %.0f s; aircraft cannot exceed about %.0f m/s².", g("accel_mps2"), g("span_s"), g("max_mps2"))
	case strings.HasPrefix(ev.Rule, "D2"):
		s := fmt.Sprintf("Geometric (GNSS) altitude differs from barometric altitude by %.0f ft", g("delta_alt_ft"))
		if b, ok := m["baseline_ft"]; ok {
			s += fmt.Sprintf(", having departed from this aircraft's own stable offset of %.0f ft by %.0f ft", b, g("deviation_ft"))
		}
		if r, ok := m["rate_ft_s"]; ok {
			s += fmt.Sprintf(", changing at %.0f ft/s", r)
		}
		return s + "."
	case strings.HasPrefix(ev.Rule, "D3"):
		s := fmt.Sprintf("A transponder identity never heard before in this session first appeared airborne %.1f NM from %s at %.0f ft above the aerodrome, "+
			"%.0f NM inside receiver coverage (so it did not fly in from outside coverage) and not near any runway.",
			g("dist_ref_nm"), ev.Geo.NearestAerodrome, g("alt_agl_ft"), g("coverage_edge_nm"))
		return s
	case strings.HasPrefix(ev.Rule, "D4"):
		s := fmt.Sprintf("The vessel has not been heard for %.0f min (expected reporting interval implies silence beyond %.0f min is abnormal). "+
			"Its last position was %.1f NM from the 200 NM EEZ limit", g("gap_s")/60, g("threshold_s")/60, math.Abs(g("signed_dist_nm")))
		if g("signed_dist_nm") > 0 {
			s += " on the high-seas side"
		} else {
			s += " inside the EEZ"
		}
		if a, ok := m["inward_angle_deg"]; ok {
			s += fmt.Sprintf(", with its last course %.0f° off the direction into the EEZ", a)
		}
		if g("dr_inside") == 1 {
			s += "; dead reckoning places it inside the EEZ now"
		}
		if c := g("course_change_deg"); c > 0 {
			s += fmt.Sprintf("; it changed course by %.0f° shortly before going silent", c)
		}
		return s + fmt.Sprintf(". AIS coverage prior for that area: %.2f (0 = vessels are regularly heard there, so a coverage gap is unlikely; 1 = vessels are never heard there).", g("coverage_prior"))
	case strings.HasPrefix(ev.Rule, "D5"):
		for _, k := range []string{"nac_p", "nic"} {
			if v, ok := m[k]; ok {
				return fmt.Sprintf("Navigation %s dropped from %.0f to %.0f within 30 s while airborne.", strings.ToUpper(k), m[k+"_before"], v)
			}
		}
	}
	return ev.Rule
}
