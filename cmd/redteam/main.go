// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command redteam renders adversary scenarios offline into a replayable JSONL
// file plus ground truth (SPEC §4.4). Output is deterministic for a given
// seed and epoch. Nothing here transmits RF.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/rx/redteam"
	"github.com/b45t3rr/lattice-mdti/internal/rx/replay"
)

func main() {
	scenarios := flag.String("scenario", "", "comma-separated scenario YAML files")
	out := flag.String("out", "redteam.jsonl", "frames output (JSONL, replay with mdti -replay)")
	truth := flag.String("truth", "groundtruth.jsonl", "ground-truth output (JSONL)")
	epochS := flag.String("epoch", "2026-01-01T00:00:00Z", "absolute time of t=0 (RFC 3339)")
	seed := flag.Int64("seed", 0, "override every scenario's seed (0 = keep)")
	flag.Parse()

	if err := run(*scenarios, *out, *truth, *epochS, *seed); err != nil {
		fmt.Fprintln(os.Stderr, "redteam:", err)
		os.Exit(1)
	}
}

func run(list, outPath, truthPath, epochS string, seed int64) error {
	epoch, err := time.Parse(time.RFC3339, epochS)
	if err != nil {
		return fmt.Errorf("epoch: %w", err)
	}
	var frames []domain.Frame
	var truth []redteam.Truth
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		s, err := redteam.LoadScenario(p)
		if err != nil {
			return err
		}
		if seed != 0 {
			s.Seed = seed
		}
		plan, err := s.Render(epoch, nil)
		if err != nil {
			return err
		}
		frames = append(frames, plan.Frames...)
		truth = append(truth, plan.Truth...)
		fmt.Fprintf(os.Stderr, "%-20s %-12s seed=%d frames=%d\n", s.Name, s.Attack, s.Seed, len(plan.Frames))
	}
	if len(frames) == 0 {
		return fmt.Errorf("no scenarios given (-scenario)")
	}
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].ObservedAt.Before(frames[j].ObservedAt) })

	fo, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer fo.Close()
	rec := replay.NewRecorder(fo)
	for _, f := range frames {
		if err := rec.Write(f); err != nil {
			return err
		}
	}
	if err := rec.Flush(); err != nil {
		return err
	}
	ft, err := os.Create(truthPath)
	if err != nil {
		return err
	}
	defer ft.Close()
	if err := redteam.WriteTruth(ft, truth); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d frames to %s and %d truth records to %s\n", len(frames), outPath, len(truth), truthPath)
	return nil
}
