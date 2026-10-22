// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command agent runs MDTI's simulated patrol assets as Lattice taskable agents
// (SPEC §8.4). It is deliberately a separate process from the daemon, so the
// task routing it exercises is Lattice's, not an in-process shortcut.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/b45t3rr/lattice-mdti/internal/agent"
	"github.com/b45t3rr/lattice-mdti/internal/config"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

func main() {
	env := flag.String("env", ".env", "optional .env file")
	b := agent.DefaultBehaviour()
	flag.DurationVar(&b.DwellVID, "dwell-vid", b.DwellVID, "time on target for visual identification")
	flag.DurationVar(&b.DwellShadow, "dwell-shadow", b.DwellShadow, "time on target for intercept-and-shadow")
	flag.DurationVar(&b.DwellRF, "dwell-rf", b.DwellRF, "time on station for RF emitter location")
	flag.DurationVar(&b.TargetLostAfter, "target-lost", b.TargetLostAfter, "fail the task when the target is not seen for this long")
	flag.Parse()
	if err := run(*env, b); err != nil {
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(1)
	}
}

func run(envFile string, b agent.Behaviour) error {
	cfg, err := config.Load(envFile)
	if err != nil {
		return err
	}
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	log := slog.New(slog.NewTextHandler(os.Stderr, opts)).With("module", "agent")
	c, err := lattice.New(cfg.Lattice)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g := &agent.Agent{Unary: c.Unary, Stream: c.Stream, Log: log}
	instance := fmt.Sprintf("%x", rand.Uint64())
	for _, a := range agent.DefaultAssets() {
		as := agent.NewAsset(a, b, c.Unary.Entities, c.Unary.Tasks, cfg.Lattice.IntegrationName, log)
		as.Instance = instance
		g.Assets = append(g.Assets, as)
		log.Info("asset ready", "name", a.Name, "id", a.ID, "home", fmt.Sprintf("%.4f,%.4f", a.Home.Lat, a.Home.Lon),
			"cruise_mps", a.CruiseMps, "serves", a.Domain)
	}
	log.Info("agent starting", "instance", instance, "auth", c.AuthMode, "dwell_vid", b.DwellVID, "dwell_shadow", b.DwellShadow)
	if err := g.Run(ctx); err != nil {
		return err
	}
	log.Info("agent stopped", "at", time.Now().Format(time.RFC3339))
	return nil
}
