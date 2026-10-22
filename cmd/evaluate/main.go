// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command evaluate replays a JSONL recording through Tier 1 offline (as fast as
// possible, on the recording's own clock) and scores the anomalies against
// red-team ground truth (SPEC §10.2). Without -truth it reports the anomaly
// rate, which on clean live traffic is the false-positive rate.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/config"
	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/rx/replay"
	"github.com/b45t3rr/lattice-mdti/internal/tier1"
)

func main() {
	framesPath := flag.String("frames", "", "JSONL frames (mdti -record or redteam -out)")
	truthPath := flag.String("truth", "", "optional ground-truth JSONL")
	out := flag.String("anomalies", "", "optional output: anomalies as JSONL")
	warmup := flag.Duration("warmup", 0, "ghost warm-up from the first frame (live default is 20s)")
	tail := flag.Duration("tail", 30*time.Minute, "advance the clock this long past the last frame (lets D4 see silence)")
	minRecall := flag.Float64("min-recall", 1, "exit non-zero when recall is below this (with -truth)")
	verbose := flag.Bool("v", false, "print every anomaly")
	flag.Parse()
	if err := run(*framesPath, *truthPath, *out, *warmup, *tail, *minRecall, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "evaluate:", err)
		os.Exit(1)
	}
}

func run(framesPath, truthPath, outPath string, warmup, tail time.Duration, minRecall float64, verbose bool) error {
	if framesPath == "" {
		return fmt.Errorf("-frames is required")
	}
	fh, err := os.Open(framesPath)
	if err != nil {
		return err
	}
	frames, err := replay.Read(fh)
	fh.Close()
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return fmt.Errorf("no frames in %s", framesPath)
	}
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].ObservedAt.Before(frames[j].ObservedAt) })

	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	zones, err := geo.LoadZones("data/geo/zones.yaml")
	if err != nil {
		return err
	}
	aerodromes, err := geo.LoadAerodromes("data/geo/aerodromes.json")
	if err != nil {
		return err
	}
	tc := tier1.DefaultConfig()
	tc.GhostWarmup = warmup
	tc.CoverageCenter = geo.Point{Lat: cfg.ADSB.CenterLat, Lon: cfg.ADSB.CenterLon}
	tc.CoverageRadiusNM = cfg.ADSB.RadiusNM
	tc.Cooldown = cfg.Tier1Cooldown
	eng := tier1.New(tc, zones, aerodromes)

	start := time.Now()
	var events []domain.AnomalyEvent
	for _, f := range frames {
		events = append(events, eng.Process(f)...)
	}
	last := frames[len(frames)-1].ObservedAt
	for t := last; !t.After(last.Add(tail)); t = t.Add(tc.DarkCheckEvery) {
		events = append(events, eng.Advance(t)...)
	}
	elapsed := time.Since(start)

	span := last.Sub(frames[0].ObservedAt)
	entities := map[string]bool{}
	for _, f := range frames {
		entities[f.EntityKey] = true
	}
	fmt.Printf("frames=%d entities=%d span=%s processed_in=%s (%.0f frames/s) ignored=%d\n",
		len(frames), len(entities), span.Round(time.Second), elapsed.Round(time.Millisecond),
		float64(len(frames))/elapsed.Seconds(), eng.Ignored)

	if outPath != "" {
		of, err := os.Create(outPath)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(of)
		for _, ev := range events {
			if err := enc.Encode(ev); err != nil {
				of.Close()
				return err
			}
		}
		of.Close()
	}
	byRule := map[string]int{}
	escalated := 0
	for _, ev := range events {
		byRule[fmt.Sprintf("%s sev%d", ev.Rule, ev.Severity)]++
		if ev.Escalate {
			escalated++
		}
		if verbose {
			fmt.Println("  " + line(ev))
		}
	}
	fmt.Printf("anomalies=%d escalated=%d", len(events), escalated)
	if hours := span.Hours(); hours > 0 {
		fmt.Printf(" (%.1f/h, escalated %.1f/h)", float64(len(events))/hours, float64(escalated)/hours)
	}
	fmt.Println()
	keys := make([]string, 0, len(byRule))
	for k := range byRule {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-34s %d\n", k, byRule[k])
	}

	if truthPath == "" {
		return nil
	}
	th, err := os.Open(truthPath)
	if err != nil {
		return err
	}
	truth, err := tier1.ReadTruth(th)
	th.Close()
	if err != nil {
		return err
	}
	rep := tier1.Score(events, truth)
	fmt.Printf("\nground truth: %d records, recall=%.2f, false positives on labelled entities=%d, unlabelled=%d\n",
		len(truth), rep.Recall, len(rep.FalsePositives), len(rep.Unlabelled))
	for _, m := range rep.Matches {
		status := "MISSED"
		if m.Event != nil {
			status = fmt.Sprintf("detected +%-6s sev%d %s", m.Latency.Round(time.Second), m.Event.Severity, m.Event.Rule)
		}
		fmt.Printf("  %-20s %-14s %-22s %s\n", m.Truth.Scenario, m.Truth.EntityKey, m.Truth.AnomalyType, status)
	}
	for _, ev := range rep.FalsePositives {
		fmt.Println("  FP " + line(ev))
	}
	if rep.Recall < minRecall {
		return fmt.Errorf("recall %.2f below %.2f", rep.Recall, minRecall)
	}
	return nil
}

func line(ev domain.AnomalyEvent) string {
	var corr []string
	for _, c := range ev.Corroborating {
		corr = append(corr, string(c))
	}
	return fmt.Sprintf("%s %-20s %-22s sev%d esc=%-5t %-24s corr=[%s]", ev.DetectedAt.UTC().Format("15:04:05"),
		ev.EntityID, ev.Type, ev.Severity, ev.Escalate, ev.Rule, strings.Join(corr, ","))
}
