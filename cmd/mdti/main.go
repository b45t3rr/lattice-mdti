// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command mdti is the MDTI daemon (SPEC §1). Implemented modules: rx (live
// ADS-B/AIS ingestion, red-team injection, record/replay) and ing (Lattice
// entity sync), tier1 (heuristic anomaly detection) and tier2 (AI decisions
// with evidence stored in Lattice Objects).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/c2"
	"github.com/b45t3rr/lattice-mdti/internal/config"
	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/evidence"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/ing"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
	"github.com/b45t3rr/lattice-mdti/internal/rx/adsb"
	"github.com/b45t3rr/lattice-mdti/internal/rx/ais"
	"github.com/b45t3rr/lattice-mdti/internal/rx/bus"
	"github.com/b45t3rr/lattice-mdti/internal/rx/redteam"
	"github.com/b45t3rr/lattice-mdti/internal/rx/replay"
	"github.com/b45t3rr/lattice-mdti/internal/rx/sbs"
	rxsources "github.com/b45t3rr/lattice-mdti/internal/rx/sources"
	"github.com/b45t3rr/lattice-mdti/internal/rx/synth"
	"github.com/b45t3rr/lattice-mdti/internal/tier1"
	"github.com/b45t3rr/lattice-mdti/internal/tier2"
	"github.com/b45t3rr/lattice-mdti/internal/viz"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type flags struct {
	envFile     string
	modules     string
	sources     string
	dryRun      bool
	record      string
	replayFile  string
	replaySpeed float64
	redteam     string
	truthFile   string
	duration    time.Duration
	anomalies   string
	decisions   string
	overlays    bool
	tasking     bool
}

func main() {
	var f flags
	flag.StringVar(&f.envFile, "env", ".env", "optional .env file (existing environment wins)")
	flag.StringVar(&f.modules, "modules", "rx,ing,tier1,tier2", "comma-separated modules to run: rx, ing, tier1, tier2")
	flag.StringVar(&f.sources, "sources", "", "comma-separated sources (default: $SOURCES, else adsb,ais): adsb, aisstream, synthetic, readsb:<url>, sbs:<host:port>, nmea-udp:<addr>, nmea-tcp:<host:port>; see docs/SOURCES.md")
	flag.BoolVar(&f.dryRun, "dry-run", false, "print every frame to stdout")
	flag.StringVar(&f.record, "record", "", "record every frame to this JSONL file")
	flag.StringVar(&f.replayFile, "replay", "", "replay this JSONL file instead of live sources")
	flag.Float64Var(&f.replaySpeed, "replay-speed", 1, "replay pacing multiplier (0 = as fast as possible, original timestamps)")
	flag.StringVar(&f.redteam, "redteam", "", "comma-separated scenario YAML files to inject")
	flag.StringVar(&f.truthFile, "truth", "groundtruth.jsonl", "ground-truth output for red-team scenarios")
	flag.DurationVar(&f.duration, "duration", 0, "stop after this long (0 = until interrupted)")
	flag.StringVar(&f.anomalies, "anomalies", "", "write Tier-1 anomalies to this JSONL file")
	flag.StringVar(&f.decisions, "decisions", "", "write Tier-2 decisions to this JSONL file")
	flag.BoolVar(&f.tasking, "tasking", true, "let C2 dispatch ISR tasks to patrol assets (false = detection only: classify, mark and draw evidence)")
	flag.BoolVar(&f.overlays, "overlays", true, "draw MDTI's background map layers (zones, coverage, 200 NM limit) in Lattice")
	flag.Parse()

	if err := run(f); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintln(os.Stderr, "mdti:", err)
		os.Exit(1)
	}
}

func run(f flags) error {
	cfg, err := config.Load(f.envFile)
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	sourcesSet := false
	flag.Visit(func(fl *flag.Flag) { sourcesSet = sourcesSet || fl.Name == "sources" })
	if !sourcesSet {
		f.sources = cfg.Sources
	}

	modules := map[string]bool{}
	for _, m := range split(f.modules) {
		switch m {
		case "rx", "ing", "tier1", "tier2":
			modules[m] = true
		default:
			return fmt.Errorf("module %q is not implemented yet (available: rx, ing, tier1, tier2)", m)
		}
	}
	if !modules["rx"] {
		return errors.New("module rx is required (it feeds every other module)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if f.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.duration)
		defer cancel()
	}

	b := bus.New(4096)
	var consumers []*bus.Subscription
	printer := b.Subscribe("dry-run", 4096)
	consumers = append(consumers, printer)
	var recorderSub *bus.Subscription
	if f.record != "" {
		recorderSub = b.Subscribe("recorder", 8192)
	}
	if modules["tier2"] && !modules["tier1"] {
		return errors.New("module tier2 needs tier1")
	}
	var clients *lattice.Clients
	if modules["ing"] || modules["tier2"] {
		if clients, err = lattice.New(cfg.Lattice); err != nil {
			if modules["ing"] {
				return fmt.Errorf("ing: %w", err)
			}
			log.Warn("Lattice not configured: Tier-2 evidence will not be uploaded", "err", err)
			clients = nil
		}
	}
	var ingSub *bus.Subscription
	var engine *ing.Engine
	var publisher *ing.Publisher
	if modules["ing"] {
		publisher = ing.NewPublisher(clients.Unary.Entities, float64(cfg.Lattice.MaxPublishPerSec), log.With("module", "ing"))
		engine = ing.NewEngine(&ing.Mapper{IntegrationName: cfg.Lattice.IntegrationName}, publisher, log)
		ingSub = b.Subscribe("ing", 8192)
		log.Info("ING enabled", "endpoint", lattice.RedactEndpoint(cfg.Lattice.Endpoint), "auth", clients.AuthMode,
			"integration", cfg.Lattice.IntegrationName, "max_publish_per_sec", cfg.Lattice.MaxPublishPerSec)
	}
	var t1Sub *bus.Subscription
	var t1 *tier1.Engine
	if modules["tier1"] {
		zones, err := geo.LoadZones("data/geo/zones.yaml")
		if err != nil {
			return fmt.Errorf("tier1: %w", err)
		}
		aerodromes, err := geo.LoadAerodromes("data/geo/aerodromes.json")
		if err != nil {
			return fmt.Errorf("tier1: %w", err)
		}
		tc := tier1.DefaultConfig()
		tc.CoverageCenter = geo.Point{Lat: cfg.ADSB.CenterLat, Lon: cfg.ADSB.CenterLon}
		tc.CoverageRadiusNM = cfg.ADSB.RadiusNM
		tc.Cooldown = cfg.Tier1Cooldown
		tc.DarkMinGap = cfg.Tier1DarkMinGap
		t1 = tier1.New(tc, zones, aerodromes)
		t1Sub = b.Subscribe("tier1", 8192)
		log.Info("Tier 1 enabled", "zones", len(zones.Zones), "ghost_refs", tc.GhostRefs, "dark_min_gap", tc.DarkMinGap)
		if tc.DarkMinGap < 10*time.Minute {
			log.Warn("DEMO PROFILE: dark-vessel silence threshold shortened", "dark_min_gap", tc.DarkMinGap, "spec_default", 10*time.Minute)
		}
	}
	var hijack *bus.Subscription
	scenarios, err := loadScenarios(f.redteam)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(scenarios, func(s *redteam.Scenario) bool { return s.Target.Mode == redteam.TargetHijack }) {
		hijack = b.Subscribe("redteam-hijack", 1024)
	}

	var wg sync.WaitGroup
	start := func(name string, fn func() error) {
		wg.Go(func() {
			if err := fn(); err != nil && ctx.Err() == nil {
				log.Error("component stopped", "component", name, "err", err)
			}
		})
	}

	var sources []func() error
	if f.replayFile != "" {
		fh, err := os.Open(f.replayFile)
		if err != nil {
			return err
		}
		frames, err := replay.Read(fh)
		fh.Close()
		if err != nil {
			return fmt.Errorf("replay %s: %w", f.replayFile, err)
		}
		log.Info("replaying", "file", f.replayFile, "frames", len(frames), "speed", f.replaySpeed)
		p := &replay.Player{Frames: frames, Speed: f.replaySpeed, Rebase: f.replaySpeed > 0}
		sources = append(sources, func() error { return p.Run(ctx, b.In()) })
	} else {
		specs, err := rxsources.Parse(f.sources)
		if err != nil {
			return err
		}
		for _, sp := range specs {
			var src interface {
				Run(context.Context, chan<- domain.Frame) error
			}
			switch sp.Kind {
			case rxsources.ADSB:
				hs, err := adsb.NewHTTPSource(cfg.ADSB.Source, cfg.ADSB.CenterLat, cfg.ADSB.CenterLon, cfg.ADSB.RadiusNM, cfg.ADSB.PollInterval, log)
				if err != nil {
					return err
				}
				log.Info("ADS-B source", "provider", hs.Name(), "center", fmt.Sprintf("%.4f,%.4f", cfg.ADSB.CenterLat, cfg.ADSB.CenterLon),
					"radius_nm", cfg.ADSB.RadiusNM, "interval", hs.Interval)
				src = hs
			case rxsources.AISStream:
				if cfg.AIS.APIKey.Reveal() == "" {
					log.Warn("AISSTREAM_API_KEY not set; AIS source disabled")
					continue
				}
				src = ais.NewStream(cfg.AIS.APIKey, cfg.AIS.BBox, cfg.AIS.IdleTimeout, log)
			case rxsources.Synthetic:
				aero, err := geo.LoadAerodromes("data/geo/aerodromes.json")
				if err != nil {
					return err
				}
				tr := &synth.Traffic{Center: geo.Point{Lat: cfg.ADSB.CenterLat, Lon: cfg.ADSB.CenterLon}, RadiusNM: cfg.ADSB.RadiusNM,
					Aircraft: 14, Seed: uint64(cfg.RedTeamSeed), Avoid: []geo.Point{aero["SADM"].Point()}, AvoidNM: 20}
				log.Info("synthetic background traffic (offline demo)", "aircraft", tr.Aircraft, "radius_nm", tr.RadiusNM)
				src = tr
			case rxsources.Readsb:
				ls, err := adsb.NewLocalSource(sp.Arg, cfg.ADSB.PollInterval, log)
				if err != nil {
					return err
				}
				log.Info("local ADS-B receiver (aircraft.json)", "url", ls.URL, "interval", ls.Interval)
				src = ls
			case rxsources.SBS:
				ss, err := sbs.New(sp.Arg, log.With("module", "rx", "source", "sbs"))
				if err != nil {
					return err
				}
				src = ss
			case rxsources.NMEAUDP, rxsources.NMEATCP:
				network := "udp"
				if sp.Kind == rxsources.NMEATCP {
					network = "tcp"
				}
				ns, err := ais.NewNMEA(network, sp.Arg, log.With("module", "rx", "source", "nmea"))
				if err != nil {
					return err
				}
				src = ns
			}
			log.Info("source enabled", "source", sp.String())
			sources = append(sources, func() error { return src.Run(ctx, b.In()) })
		}
	}

	var truthW io.Writer
	if len(scenarios) > 0 {
		tf, err := os.Create(f.truthFile)
		if err != nil {
			return err
		}
		defer tf.Close()
		truthW = &syncWriter{w: tf}
	}
	for _, s := range scenarios {
		r := &redteam.Runner{Scenario: s, Log: log, Truth: truthW}
		var live <-chan domain.Frame
		if hijack != nil && s.Target.Mode == redteam.TargetHijack {
			live = hijack.C
		}
		sources = append(sources, func() error { return r.Run(ctx, b.In(), live) })
	}
	if len(sources) == 0 {
		return errors.New("no sources: enable -sources, -replay or -redteam")
	}

	wg.Go(func() { b.Run(ctx) })
	for i, src := range sources {
		start(fmt.Sprintf("source-%d", i), src)
	}
	if hijack != nil {
		wg.Go(func() { drainAfterPick(ctx, hijack) })
	}
	if recorderSub != nil {
		start("recorder", func() error { return record(f.record, recorderSub, log) })
	}
	if t1 != nil {
		anomalies := make(chan domain.AnomalyEvent, 256)
		wallClock := f.replayFile == "" || f.replaySpeed > 0
		start("tier1", func() error { t1.Run(ctx, t1Sub.C, anomalies, wallClock, log.With("module", "tier1")); return nil })
		var t2In chan domain.AnomalyEvent
		if modules["tier2"] {
			t2, err := newTier2(cfg, log)
			if err != nil {
				return fmt.Errorf("tier2: %w", err)
			}
			var ctrl *c2.Controller
			if clients != nil {
				ctrl = &c2.Controller{Entities: clients.Unary.Entities, Tasks: clients.Unary.Tasks, Assets: t2.Policy.Assets,
					Integration: cfg.Lattice.IntegrationName, Log: log.With("module", "c2"), DetectionOnly: !f.tasking}
				if engine != nil {
					ctrl.ING = engine
				}
				log.Info("C2 enabled", "assets", len(t2.Policy.Assets), "autonomy", cfg.Autonomy, "tasking", f.tasking)
				start("c2-markers", func() error { ctrl.Run(ctx); return nil })
			}
			t2In = make(chan domain.AnomalyEvent, 256)
			results := make(chan tier2.Result, 64)
			start("tier2", func() error { t2.Run(ctx, t2In, results); return nil })
			start("decision-sink", func() error { return sinkDecisions(ctx, results, clients, ctrl, f.decisions, log) })
		}
		start("anomaly-sink", func() error { return sinkAnomalies(ctx, anomalies, f.anomalies, t2In) })
	}
	if clients != nil && f.overlays {
		start("overlays", func() error { publishOverlays(ctx, cfg, clients, log); return nil })
	}
	if engine != nil {
		// The publisher outlives the engine so the final flush still reaches Lattice.
		pubCtx, pubCancel := context.WithCancel(context.Background())
		var pubWG sync.WaitGroup
		pubWG.Go(func() { publisher.Run(pubCtx) })
		start("ing", func() error { engine.Run(ctx, ingSub.C); return nil })
		start("ing-stats", func() error { logPublisherStats(ctx, publisher, log); return nil })
		defer func() { pubCancel(); pubWG.Wait(); logPublisher(publisher, log, "ing final") }()
	}

	stats := newStats()
	for _, c := range consumers {
		start("printer", func() error { return consume(ctx, c, f.dryRun, stats, log) })
	}
	<-ctx.Done()
	wg.Wait()
	stats.log(log, "final")
	for _, c := range append(consumers, recorderSub, ingSub, t1Sub) {
		if c != nil && c.Dropped.Load() > 0 {
			log.Warn("consumer dropped frames", "consumer", c.Name, "dropped", c.Dropped.Load())
		}
	}
	return ctx.Err()
}

// drainAfterPick keeps the hijack subscription from filling up once runners
// have picked their target (the bus would count drops otherwise).
func drainAfterPick(ctx context.Context, s *bus.Subscription) {
	<-ctx.Done()
	for range s.C {
	}
}

func consume(ctx context.Context, s *bus.Subscription, print bool, st *stats, log *slog.Logger) error {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			st.log(log, "rx stats")
		case f, ok := <-s.C:
			if !ok {
				return nil
			}
			st.add(f)
			if print {
				fmt.Println(formatFrame(f))
			}
		}
	}
}

// sinkAnomalies writes Tier-1 output to JSONL (optional) and forwards it to
// Tier 2 when enabled.
func sinkAnomalies(ctx context.Context, in <-chan domain.AnomalyEvent, path string, next chan<- domain.AnomalyEvent) error {
	var enc *json.Encoder
	if path != "" {
		fh, err := os.Create(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		enc = json.NewEncoder(fh)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-in:
			if enc != nil {
				if err := enc.Encode(ev); err != nil {
					return err
				}
			}
			if next != nil {
				select {
				case next <- ev:
				case <-ctx.Done():
					return nil
				}
			}
		}
	}
}

func newTier2(cfg *config.Config, log *slog.Logger) (*tier2.Engine, error) {
	if cfg.OpenAI.APIKey.Reveal() == "" {
		return nil, tier2.ErrNotConfigured
	}
	pol, err := tier2.LoadPolicy("data/tier2/policy.yaml")
	if err != nil {
		return nil, err
	}
	zones, err := geo.LoadZones("data/geo/zones.yaml")
	if err != nil {
		return nil, err
	}
	client := openai.NewClient(option.WithAPIKey(cfg.OpenAI.APIKey.Reveal()))
	sum := sha256.Sum256([]byte("mdti|" + cfg.Lattice.Endpoint))
	log.Info("Tier 2 enabled", "model", cfg.OpenAI.DecisionsModel, "policy", pol.Version, "autonomy", cfg.Autonomy,
		"confirm", pol.Thresholds.Confirm, "hostile", pol.Thresholds.Hostile)
	return &tier2.Engine{
		API: &client.Decisions, Policy: pol, Model: cfg.OpenAI.DecisionsModel, Autonomy: cfg.Autonomy, Zones: zones,
		Concurrency: cfg.Tier2Concurrency, PerMinute: cfg.Tier2PerMinute, SafetyID: hex.EncodeToString(sum[:8]),
		Log: log.With("module", "tier2"),
	}, nil
}

// sinkDecisions logs each decision, uploads its evidence to Lattice Objects
// hands it to C2 (override, annotation, tasking) and optionally writes JSONL.
func sinkDecisions(ctx context.Context, in <-chan tier2.Result, clients *lattice.Clients, ctrl *c2.Controller, path string, log *slog.Logger) error {
	var enc *json.Encoder
	if path != "" {
		fh, err := os.Create(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		enc = json.NewEncoder(fh)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-in:
			d := r.Decision
			evPath := ""
			if clients != nil {
				uctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				p, err := evidence.Upload(uctx, clients.Unary.Objects, r)
				cancel()
				if err != nil {
					log.Warn("evidence upload failed", "entity", d.EntityID, "err", lattice.Describe(err))
				} else {
					evPath = p
				}
			}
			log.Info("decision", "module", "tier2", "entity", d.EntityID, "confirmed", d.AnomalyConfirmed,
				"p", fmt.Sprintf("%.2f", d.ConfidenceScore), "category", d.ThreatCategory, "recommend", d.Disposition,
				"action", d.Action.Disposition, "task", d.RecommendedTask, "dispatch", d.Action.DispatchTask,
				"fallback", d.Audit.Fallback, "cached", d.Audit.Cached, "latency_ms", d.Audit.LatencyMs, "evidence", evPath,
				"log", d.TacticalReasoning)
			var outcome c2.Outcome
			if ctrl != nil {
				outcome = ctrl.Handle(ctx, d, r.Event, evPath)
				log.Info("c2", "entity", d.EntityID, "overridden", outcome.Overridden, "annotated", outcome.Annotated,
					"dead_reckoning", outcome.DeadReckoned, "task", outcome.TaskID, "asset", outcome.Asset, "skipped", outcome.Skipped,
					"evidence", outcome.Evidence)
			}
			if enc != nil {
				if err := enc.Encode(struct {
					tier2.Result
					EvidencePath string     `json:"evidence_path,omitempty"`
					C2           c2.Outcome `json:"c2"`
				}{r, evPath, outcome}); err != nil {
					return err
				}
			}
		}
	}
}

// publishOverlays keeps MDTI's static map layers alive in Lattice while the
// daemon runs; they expire a few minutes after it stops.
func publishOverlays(ctx context.Context, cfg *config.Config, c *lattice.Clients, log *slog.Logger) {
	zones, err := geo.LoadZones("data/geo/zones.yaml")
	if err != nil {
		log.Warn("overlays disabled", "err", err)
		return
	}
	aerodromes, err := geo.LoadAerodromes("data/geo/aerodromes.json")
	if err != nil {
		log.Warn("overlays disabled", "err", err)
		return
	}
	layers := viz.Overlays(zones, aerodromes, geo.Point{Lat: cfg.ADSB.CenterLat, Lon: cfg.ADSB.CenterLon}, cfg.ADSB.RadiusNM,
		tier1.DefaultConfig().GhostRadiusNM)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		now := time.Now().UTC()
		for _, l := range layers {
			if _, err := c.Unary.Entities.PublishEntity(ctx, l.Entity(cfg.Lattice.IntegrationName, now, now.Add(3*time.Minute))); err != nil && ctx.Err() == nil {
				log.Warn("overlay publish failed", "layer", l.ID, "err", lattice.Describe(err))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func logPublisherStats(ctx context.Context, p *ing.Publisher, log *slog.Logger) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			logPublisher(p, log, "ing stats")
		}
	}
}

func logPublisher(p *ing.Publisher, log *slog.Logger, msg string) {
	log.Info(msg, "published", p.Published.Load(), "pending", p.Pending(), "superseded", p.Superseded.Load(),
		"rejected", p.Rejected.Load(), "failed", p.Failed.Load(), "rate_limited", p.RateLimited.Load())
}

func record(path string, s *bus.Subscription, log *slog.Logger) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	rec := replay.NewRecorder(fh)
	n := 0
	for f := range s.C {
		if err := rec.Write(f); err != nil {
			return err
		}
		n++
	}
	log.Info("recording closed", "file", path, "frames", n)
	return rec.Flush()
}

func formatFrame(f domain.Frame) string {
	opt := func(p *float64, format string) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprintf(format, *p)
	}
	ts := f.ObservedAt.UTC().Format("15:04:05.0")
	switch f.Domain {
	case domain.DomainAir:
		gs := "-"
		if f.GroundSpeedMps != nil {
			gs = fmt.Sprintf("%.0fkt", *f.GroundSpeedMps/domain.KnotToMps)
		}
		ground := ""
		if f.OnGround != nil && *f.OnGround {
			ground = " GND"
		}
		return fmt.Sprintf("%s %-9s AIR %-12s %-8s %9.4f %9.4f baro=%s geom=%s gs=%s trk=%s nacp=%s sqk=%s%s",
			ts, f.Source, f.EntityKey, f.Callsign, f.Lat, f.Lon, opt(f.AltBaroFt, "%.0f"), opt(f.AltGeomFt, "%.0f"),
			gs, opt(f.TrackDeg, "%.0f"), optInt(f.NACp), f.Squawk, ground)
	default:
		sog := "-"
		if f.GroundSpeedMps != nil {
			sog = fmt.Sprintf("%.1fkn", *f.GroundSpeedMps/domain.KnotToMps)
		}
		return fmt.Sprintf("%s %-9s SEA %-14s %-20q %9.4f %9.4f sog=%s cog=%s class=%s type=%s",
			ts, f.Source, f.EntityKey, f.ShipName, f.Lat, f.Lon, sog, opt(f.TrackDeg, "%.0f"), f.AISClass, optInt(f.ShipType))
	}
}

func optInt(p *int) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

type stats struct {
	mu       sync.Mutex
	frames   map[domain.Source]int
	entities map[string]struct{}
}

func newStats() *stats {
	return &stats{frames: map[domain.Source]int{}, entities: map[string]struct{}{}}
}

func (s *stats) add(f domain.Frame) {
	s.mu.Lock()
	s.frames[f.Source]++
	s.entities[f.EntityKey] = struct{}{}
	s.mu.Unlock()
}

func (s *stats) log(log *slog.Logger, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	args := []any{"entities", len(s.entities)}
	for src, n := range s.frames {
		args = append(args, "frames_"+string(src), n)
	}
	log.Info(msg, args...)
}

func loadScenarios(list string) ([]*redteam.Scenario, error) {
	var out []*redteam.Scenario
	for _, p := range split(list) {
		s, err := redteam.LoadScenario(p)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
