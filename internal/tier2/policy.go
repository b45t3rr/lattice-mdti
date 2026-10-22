// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package tier2

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// Thresholds are the calibrated decision cut-offs (SPEC §7.5).
type Thresholds struct {
	Confirm      float64 `yaml:"confirm"`
	Hostile      float64 `yaml:"hostile"`
	Category     float64 `yaml:"category"`
	Task         float64 `yaml:"task"`
	LevelHostile float64 `yaml:"level_hostile"`
	LevelSuspect float64 `yaml:"level_suspect"`
}

// Asset is a patrol asset home used for time-to-target estimates.
type Asset struct {
	ID       string        `yaml:"id"`
	Domain   domain.Domain `yaml:"domain"`
	Lat      float64       `yaml:"lat"`
	Lon      float64       `yaml:"lon"`
	SpeedMps float64       `yaml:"speed_mps"`
}

// Policy is the deterministic half of Tier 2.
type Policy struct {
	QuestionSet    string        `yaml:"question_set"`
	Thresholds     Thresholds    `yaml:"thresholds"`
	FlapProtection time.Duration `yaml:"flap_protection"`
	CacheTTL       time.Duration `yaml:"cache_ttl"`
	Theater        struct {
		MinLat float64 `yaml:"min_lat"`
		MaxLat float64 `yaml:"max_lat"`
		MinLon float64 `yaml:"min_lon"`
		MaxLon float64 `yaml:"max_lon"`
	} `yaml:"theater"`
	Assets      []Asset   `yaml:"assets"`
	Calibration yaml.Node `yaml:"calibration"`

	Version string `yaml:"-"` // "policy-<sha256[:12]>"
}

// LoadPolicy reads data/tier2/policy.yaml.
func LoadPolicy(path string) (*Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if p.QuestionSet != QuestionSetVersion {
		return nil, fmt.Errorf("%s: calibrated for question set %q, engine uses %q (re-run cmd/calibrate)", path, p.QuestionSet, QuestionSetVersion)
	}
	t := p.Thresholds
	for name, v := range map[string]float64{"confirm": t.Confirm, "hostile": t.Hostile, "category": t.Category, "task": t.Task} {
		if v <= 0 || v > 1 {
			return nil, fmt.Errorf("%s: threshold %s=%v out of (0,1]", path, name, v)
		}
	}
	if t.Hostile < t.Confirm {
		return nil, fmt.Errorf("%s: hostile threshold below confirm threshold", path)
	}
	sum := sha256.Sum256(raw)
	p.Version = "policy-" + hex.EncodeToString(sum[:6])
	return &p, nil
}

// Context is everything outside the model answers that a decision depends on.
type Context struct {
	Autonomy   string          // advisory | supervised | autonomous
	ZonePolicy *geo.ZonePolicy // policy of the zone involved, if any
	Now        time.Time
}

// recommend applies thresholds and the task table (SPEC §7.5).
func (p *Policy) recommend(ev domain.AnomalyEvent, a Answers) domain.TacticalDecision {
	t := p.Thresholds
	d := domain.TacticalDecision{
		AnomalyID: ev.ID, EntityID: ev.EntityID, ConfidenceScore: a.PConfirmed, ThreatCategory: a.Category,
		RecommendedTask: domain.TaskNone,
	}
	var guards []string
	cat, guard := EffectiveCategory(a, t.Category)
	d.ThreatCategory = cat
	if guard != "" {
		guards = append(guards, guard)
	}
	d.AnomalyConfirmed = a.PConfirmed >= t.Confirm
	if a.Category == domain.ThreatBenignArtifact && a.CategoryConfidence >= t.Category && d.AnomalyConfirmed {
		d.AnomalyConfirmed = false // contradictory answers: the conservative reading wins
		guards = append(guards, "benign_category_overrides_confirmation")
	}
	switch {
	case d.AnomalyConfirmed && a.PConfirmed >= t.Hostile && a.LevelScore >= t.LevelHostile &&
		cat != domain.ThreatBenignArtifact && cat != domain.ThreatUnknown:
		d.Disposition = domain.DispositionHostile
	case d.AnomalyConfirmed && a.LevelScore >= t.LevelSuspect:
		d.Disposition = domain.DispositionSuspect
	}
	if d.Disposition != "" {
		d.RecommendedTask = taskFor(cat, ev.Domain, d.Disposition)
	}
	if d.RecommendedTask != domain.TaskNone {
		d.TargetCoordinates = p.target(ev, d.RecommendedTask)
	}
	d.Audit.GuardrailsApplied = guards
	return d
}

// categoryFamilies group categories that lead to the same actions. The live
// model splits probability between them on dark fishing vessels
// (IUU_FISHING 0.52 / AIS_DARK 0.41), which left neither above the confidence
// threshold although the family clearly was.
var categoryFamilies = [][]domain.ThreatCategory{
	{domain.ThreatAISDark, domain.ThreatIUUFishing},
}

// EffectiveCategory applies the category confidence threshold: the chosen
// category if confident; else the most probable member of a family whose
// combined probability is confident; else UNKNOWN (review queue).
func EffectiveCategory(a Answers, threshold float64) (domain.ThreatCategory, string) {
	if a.CategoryConfidence >= threshold {
		return a.Category, ""
	}
	for _, fam := range categoryFamilies {
		sum, best, bestP := 0.0, domain.ThreatCategory(""), -1.0
		for _, c := range fam {
			p := a.CategoryProbs[string(c)]
			sum += p
			if p > bestP {
				best, bestP = c, p
			}
		}
		if sum >= threshold {
			return best, "category_family_confidence"
		}
	}
	return domain.ThreatUnknown, "category_below_confidence→UNKNOWN"
}

// taskFor is the SPEC §7.5 task table.
func taskFor(cat domain.ThreatCategory, dom domain.Domain, disp domain.Disposition) domain.RecommendedTask {
	hostile := disp == domain.DispositionHostile
	switch cat {
	case domain.ThreatGNSSSpoofing:
		return domain.TaskRFEmitterLocate
	case domain.ThreatADSBGhost:
		return domain.TaskVisualIdentification
	case domain.ThreatKinematicSpoof, domain.ThreatAISDark, domain.ThreatIUUFishing:
		if hostile {
			return domain.TaskInterceptAndShadow
		}
		return domain.TaskVisualIdentification
	}
	return domain.TaskNone
}

// target computes deterministic task coordinates (never model-generated).
func (p *Policy) target(ev domain.AnomalyEvent, task domain.RecommendedTask) *domain.Coordinates {
	if len(ev.Window) == 0 {
		return nil
	}
	last := ev.Window[len(ev.Window)-1]
	if task == domain.TaskRFEmitterLocate {
		// A spoofed position says nothing about the emitter: search around the
		// centroid of the affected track.
		var lat, lon float64
		for _, f := range ev.Window {
			lat += f.Lat
			lon += f.Lon
		}
		n := float64(len(ev.Window))
		return &domain.Coordinates{Lat: lat / n, Lon: lon / n}
	}
	pos := geo.Point{Lat: last.Lat, Lon: last.Lon}
	elapsed := ev.DetectedAt.Sub(last.ObservedAt).Seconds() // > 0 for dark vessels
	predict := func(lead float64) geo.Point {
		if last.TrackDeg == nil || last.GroundSpeedMps == nil {
			return pos
		}
		return geo.DeadReckon(pos, *last.TrackDeg, *last.GroundSpeedMps, elapsed+lead)
	}
	at := predict(0)
	if a, ok := p.asset(ev.Domain); ok && last.TrackDeg != nil && last.GroundSpeedMps != nil {
		if t, ok := interceptTime(geo.Point{Lat: a.Lat, Lon: a.Lon}, a.SpeedMps, at, *last.TrackDeg, *last.GroundSpeedMps); ok {
			at = predict(math.Min(t, 1800))
		}
		// No solution: the target outruns the asset, so it is pursued from its
		// current (dead-reckoned) position.
	}
	c := &domain.Coordinates{Lat: at.Lat, Lon: at.Lon}
	if last.AltGeomFt != nil {
		c.AltHaeM = domain.Ptr(*last.AltGeomFt * domain.FtToM)
	}
	return c
}

// interceptTime solves |D + V·t| = s·t for the earliest t > 0, where D is the
// target's offset from the asset and V its constant velocity (local ENU).
func interceptTime(home geo.Point, assetSpeed float64, target geo.Point, trackDeg, speed float64) (float64, bool) {
	dx, dy := geo.ENU(home, target)
	vx, vy := geo.VelocityENU(speed, trackDeg)
	a := vx*vx + vy*vy - assetSpeed*assetSpeed
	b := 2 * (dx*vx + dy*vy)
	c := dx*dx + dy*dy
	if math.Abs(a) < 1e-9 { // equal speeds: linear
		if b >= 0 {
			return 0, false
		}
		return -c / b, true
	}
	disc := b*b - 4*a*c
	if disc < 0 {
		return 0, false
	}
	sq := math.Sqrt(disc)
	best := math.Inf(1)
	for _, t := range []float64{(-b - sq) / (2 * a), (-b + sq) / (2 * a)} {
		if t > 0 && t < best {
			best = t
		}
	}
	return best, !math.IsInf(best, 1)
}

func (p *Policy) asset(d domain.Domain) (Asset, bool) {
	for _, a := range p.Assets {
		if a.Domain == d && a.SpeedMps > 0 {
			return a, true
		}
	}
	return Asset{}, false
}

func (p *Policy) inTheater(c *domain.Coordinates) bool {
	t := p.Theater
	return c != nil && c.Lat >= t.MinLat && c.Lat <= t.MaxLat && c.Lon >= t.MinLon && c.Lon <= t.MaxLon
}

// isrTasks is the hard-coded allow-list: nothing else can ever be emitted.
var isrTasks = map[domain.RecommendedTask]bool{
	domain.TaskVisualIdentification: true, domain.TaskInterceptAndShadow: true, domain.TaskRFEmitterLocate: true,
}

var taskRank = map[domain.RecommendedTask]int{
	domain.TaskNone: 0, domain.TaskVisualIdentification: 1, domain.TaskRFEmitterLocate: 2, domain.TaskInterceptAndShadow: 3,
}

// authorize turns a recommendation into the action C2 may take (SPEC §7.7):
// guardrails first, then zone policy, then the autonomy level last.
func (p *Policy) authorize(d *domain.TacticalDecision, ctx Context) {
	g := &d.Audit.GuardrailsApplied
	act := domain.Action{Disposition: d.Disposition}
	task := d.RecommendedTask

	if act.Disposition == domain.DispositionFriendly {
		act.Disposition = "" // only MDTI's own assets are FRIENDLY
		*g = append(*g, "friendly_never_assigned_to_tracks")
	}
	if task != domain.TaskNone {
		switch {
		case !isrTasks[task]:
			task = domain.TaskNone
			*g = append(*g, "task_not_in_isr_allow_list")
		case d.ConfidenceScore < p.Thresholds.Task:
			task = domain.TaskNone
			*g = append(*g, "task_below_confidence")
		case !p.inTheater(d.TargetCoordinates):
			task = domain.TaskNone
			*g = append(*g, "task_target_outside_theater")
		}
	}
	if zp := ctx.ZonePolicy; zp != nil {
		if zp.HostileRequiresOperator && act.Disposition == domain.DispositionHostile {
			act.Disposition = domain.DispositionSuspect
			*g = append(*g, "zone_hostile_requires_operator")
		}
		if max := domain.RecommendedTask(zp.MaxAutonomousTask); max != "" && taskRank[task] > taskRank[max] {
			task = max
			*g = append(*g, "zone_caps_task_at_"+string(max))
		}
	}
	switch ctx.Autonomy {
	case "advisory":
		if act.Disposition != "" || task != domain.TaskNone {
			*g = append(*g, "autonomy_advisory")
		}
		act.Disposition, task = "", domain.TaskNone
	case "supervised":
		if act.Disposition == domain.DispositionHostile {
			act.Disposition = domain.DispositionSuspect
			*g = append(*g, "autonomy_supervised_hostile_is_recommendation")
		}
		if task != domain.TaskNone {
			*g = append(*g, "autonomy_supervised_task_is_recommendation")
		}
		task = domain.TaskNone
	}
	act.DispatchTask = task != domain.TaskNone
	if act.DispatchTask && task != d.RecommendedTask {
		d.RecommendedTask = task // the authorised (capped) task is what C2 dispatches
	}
	d.Action = act
}

// Decide turns validated answers into a full decision.
func (p *Policy) Decide(ev domain.AnomalyEvent, a Answers, model string, ctx Context) domain.TacticalDecision {
	d := p.recommend(ev, a)
	d.Audit.Model, d.Audit.QuestionSetVersion, d.Audit.PolicyVersion = model, QuestionSetVersion, p.Version
	d.Audit.PConfirmed, d.Audit.CategoryChoice = a.PConfirmed, string(a.Category)
	d.Audit.CategoryProbs, d.Audit.CategoryConfidence = a.CategoryProbs, a.CategoryConfidence
	d.Audit.ThreatLevelScore, d.Audit.ThreatLevelProbs = a.LevelScore, a.LevelProbs
	d.Audit.InputTokens, d.Audit.DecidedAt = a.InputTokens, ctx.Now
	p.authorize(&d, ctx)
	d.TacticalReasoning = operatorLog(ev, d, a.LevelScore)
	return d
}

// Fallback is the deterministic decision used when Tier 2 is unavailable
// (SPEC §7.9). It can never produce HOSTILE or a task.
func (p *Policy) Fallback(ev domain.AnomalyEvent, reason string, ctx Context) domain.TacticalDecision {
	d := domain.TacticalDecision{
		AnomalyID: ev.ID, EntityID: ev.EntityID, ThreatCategory: domain.ThreatUnknown, RecommendedTask: domain.TaskNone,
	}
	if ev.Severity >= 3 {
		d.AnomalyConfirmed, d.Disposition, d.ConfidenceScore = true, domain.DispositionSuspect, 0.5
	}
	d.Audit.QuestionSetVersion, d.Audit.PolicyVersion = QuestionSetVersion, p.Version
	d.Audit.Fallback, d.Audit.DecidedAt = reason, ctx.Now
	if d.Disposition != "" && ctx.Autonomy != "advisory" {
		d.Action.Disposition = d.Disposition
	}
	d.TacticalReasoning = truncate(fmt.Sprintf("Tier-2 unavailable (%s); Tier-1 %s sev%d (%s)", reason, ev.Type, ev.Severity, ev.Rule), 400)
	return d
}

// operatorLog renders the deterministic operator message (SPEC §7.6). It never
// includes untrusted identifiers.
func operatorLog(ev domain.AnomalyEvent, d domain.TacticalDecision, level float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", ev.Type)
	if ev.Geo.NearestAerodrome != "" && ev.Domain == domain.DomainAir {
		if ev.Geo.DistToAerodromeNM <= 25 {
			fmt.Fprintf(&b, " near %s (%.1f NM)", ev.Geo.NearestAerodrome, ev.Geo.DistToAerodromeNM)
		} else {
			fmt.Fprintf(&b, " %.0f NM from %s", ev.Geo.DistToAerodromeNM, ev.Geo.NearestAerodrome)
		}
	}
	if ev.Geo.SignedDistToEEZNM != nil {
		fmt.Fprintf(&b, " %.1f NM from mile 200", *ev.Geo.SignedDistToEEZNM)
		if ev.Geo.Zone != nil {
			fmt.Fprintf(&b, " in %s", ev.Geo.Zone.ID)
		}
	}
	fmt.Fprintf(&b, ": %s", ev.Rule)
	if k, v, ok := keyMetric(ev); ok {
		fmt.Fprintf(&b, " (%s %.0f)", k, v)
	}
	if len(ev.Corroborating) > 0 {
		fmt.Fprintf(&b, " +%d corroborating", len(ev.Corroborating))
	}
	fmt.Fprintf(&b, ". P(manip)=%.2f · %s · level %.1f", d.ConfidenceScore, d.ThreatCategory, level)
	switch {
	case d.Action.Disposition != "":
		fmt.Fprintf(&b, " → %s", d.Action.Disposition)
	case d.Disposition != "":
		fmt.Fprintf(&b, " → recommend %s", d.Disposition)
	default:
		b.WriteString(" → no change")
	}
	if d.Action.DispatchTask {
		fmt.Fprintf(&b, " · %s tasked", d.RecommendedTask)
	} else if d.RecommendedTask != domain.TaskNone {
		fmt.Fprintf(&b, " · recommend %s", d.RecommendedTask)
	}
	return truncate(b.String(), 400)
}

func keyMetric(ev domain.AnomalyEvent) (string, float64, bool) {
	for _, k := range []string{"implied_speed_mps", "delta_alt_ft", "dist_ref_nm", "gap_s", "nac_p"} {
		if v, ok := ev.Metrics[k]; ok {
			return k, v, true
		}
	}
	return "", 0, false
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
