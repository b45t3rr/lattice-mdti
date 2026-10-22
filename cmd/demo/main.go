// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command demo runs the recorded end-to-end demonstration (README "Demo"):
// it starts the agent and the daemon as separate processes, narrates the
// pipeline from their logs, follows the dispatched task itself, shows the
// final Lattice state and cleans up. With -cast it records an asciicast v2
// file that tools/cast2gif turns into the README GIF.
//
// Build first (`make build`): the demo runs bin/agent and bin/mdti.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/agent"
	"github.com/b45t3rr/lattice-mdti/internal/config"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

const target = "mdti-adsb-e3f00d"

type options struct {
	env, bin, scenario, cast, logs string
	dwell, timeout                 time.Duration
	keepOverride                   bool
}

func main() {
	var o options
	flag.StringVar(&o.env, "env", ".env", "optional .env file")
	flag.StringVar(&o.bin, "bin", "bin", "directory with the agent and mdti executables")
	flag.StringVar(&o.scenario, "scenario", "scenarios/demo_ghost.yaml", "red-team scenario to inject")
	flag.StringVar(&o.cast, "cast", "", "record the narration to this asciicast v2 file")
	flag.StringVar(&o.logs, "logs", "demo-logs", "directory for the child processes' logs")
	flag.DurationVar(&o.dwell, "dwell", 45*time.Second, "time on target for the visual identification")
	flag.DurationVar(&o.timeout, "timeout", 10*time.Minute, "abort after this long")
	flag.BoolVar(&o.keepOverride, "keep-override", false, "leave the HOSTILE override in Lattice afterwards")
	flag.Parse()

	n := newNarrator(os.Stdout)
	if o.cast != "" {
		if err := n.record(o.cast); err != nil {
			fmt.Fprintln(os.Stderr, "demo:", err)
			os.Exit(1)
		}
	}
	err := run(o, n)
	n.close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// narration

const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[90m"
	cRed    = "\x1b[91m"
	cGreen  = "\x1b[92m"
	cYellow = "\x1b[93m"
	cBlue   = "\x1b[94m"
	cMag    = "\x1b[95m"
	cCyan   = "\x1b[96m"
)

type narrator struct {
	mu    sync.Mutex
	out   io.Writer
	cast  *os.File
	start time.Time
}

func newNarrator(w io.Writer) *narrator { return &narrator{out: w, start: time.Now()} }

func (n *narrator) record(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n.cast = f
	hdr, _ := json.Marshal(map[string]any{
		"version": 2, "width": 118, "height": 38, "timestamp": n.start.Unix(),
		"title": "MDTI on Anduril Lattice — ghost aircraft near Morón, end to end",
	})
	_, err = fmt.Fprintf(f, "%s\n", hdr)
	return err
}

func (n *narrator) close() {
	if n.cast != nil {
		n.cast.Close()
	}
}

// line prints one narrated line, prefixed with the elapsed time.
func (n *narrator) line(color, stage, format string, a ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	el := time.Since(n.start)
	stamp := fmt.Sprintf("%02d:%02d", int(el.Minutes()), int(el.Seconds())%60)
	text := fmt.Sprintf(format, a...)
	s := fmt.Sprintf("%s%s%s %s%-8s%s %s\r\n", cDim, stamp, cReset, color+cBold, stage, cReset, text)
	if stage == "" {
		s = fmt.Sprintf("%s\r\n", text)
	}
	fmt.Fprint(n.out, strings.ReplaceAll(s, "\r\n", "\n"))
	if n.cast != nil {
		ev, _ := json.Marshal([]any{el.Seconds(), "o", s})
		fmt.Fprintf(n.cast, "%s\n", ev)
	}
}

// ---------------------------------------------------------------------------
// child processes

type child struct {
	name  string
	cmd   *exec.Cmd
	lines chan map[string]string
	done  chan struct{}
}

func startChild(ctx context.Context, name, exe string, args, env []string, logDir string) (*child, error) {
	if _, err := os.Stat(exe); err != nil {
		return nil, fmt.Errorf("%s not found (run `make build` first): %w", exe, err)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(os.Environ(), env...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	logf, err := os.Create(filepath.Join(logDir, name+".log"))
	if err != nil {
		return nil, err
	}
	cmd.Stdout = logf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{name: name, cmd: cmd, lines: make(chan map[string]string, 1024), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer logf.Close()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			fmt.Fprintln(logf, sc.Text())
			select {
			case c.lines <- parseKV(sc.Text()):
			default:
			}
		}
	}()
	return c, nil
}

func (c *child) stop() {
	if c == nil || c.cmd.Process == nil {
		return
	}
	_ = c.cmd.Process.Kill()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
	_ = c.cmd.Wait()
}

var kvRe = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S+)`)

// parseKV parses one slog text line into its fields.
func parseKV(line string) map[string]string {
	m := map[string]string{}
	for _, g := range kvRe.FindAllStringSubmatch(line, -1) {
		v := g[2]
		if strings.HasPrefix(v, `"`) {
			var s string
			if json.Unmarshal([]byte(v), &s) == nil {
				v = s
			}
		}
		m[g[1]] = v
	}
	return m
}

// ---------------------------------------------------------------------------
// the demo

func run(o options, n *narrator) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	n.line("", "", "%sMDTI · Multi-Domain Airspace & Maritime Threat Intelligence on Anduril Lattice%s", cBold, cReset)
	n.line("", "", "%sDefensive R&D demo · Lattice Sandbox · red team injects at the data layer only (no RF)%s", cDim, cReset)
	n.line("", "", "")

	cfg, err := config.Load(o.env)
	if err != nil {
		return err
	}
	clients, err := lattice.New(cfg.Lattice)
	if err != nil {
		return err
	}
	if _, err := clients.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: "mdti-demo-ping"}); err != nil && lattice.StatusCode(err) != 404 {
		return fmt.Errorf("lattice: %s", lattice.Describe(err))
	}
	n.line(cGreen, "SETUP", "Lattice Sandbox authenticated (%s)", lattice.RedactEndpoint(cfg.Lattice.Endpoint))
	if cfg.OpenAI.APIKey.Reveal() == "" {
		return errors.New("OPENAI_API_KEY is required for Tier 2")
	}
	for _, a := range agent.DefaultAssets() {
		probe := agent.NewAsset(a, agent.DefaultBehaviour(), clients.Unary.Entities, clients.Unary.Tasks, "", discard())
		probe.Instance = "demo-preflight"
		if err := probe.CheckExclusive(ctx); err != nil {
			return fmt.Errorf("%w — stop the other agent first", err)
		}
	}
	n.line(cGreen, "SETUP", "no other agent instance is driving SENTINEL-1 / TRIDENT-2 (single-writer lock)")

	if err := os.MkdirAll(o.logs, 0o755); err != nil {
		return err
	}
	exe := func(name string) string {
		p := filepath.Join(o.bin, name)
		if _, err := os.Stat(p + ".exe"); err == nil {
			return p + ".exe"
		}
		return p
	}

	ag, err := startChild(ctx, "agent", exe("agent"), []string{"-env", o.env, "-dwell-vid", o.dwell.String()}, nil, o.logs)
	if err != nil {
		return err
	}
	defer ag.stop()
	var md *child
	defer func() { md.stop() }()

	var (
		injectedAt, detectedAt, decidedAt, dispatchedAt, doneAt time.Time
		taskID                                                  string
		lastStatus                                              string
		agentReady, mdtiStarted, announcedPicture               bool
	)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastProgress := time.Time{}

	for doneAt.IsZero() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("demo did not complete: %w", ctx.Err())
		case <-ag.done:
			return errors.New("agent exited unexpectedly (see demo-logs/agent.log)")
		case f := <-ag.lines:
			switch f["msg"] {
			case "listening for tasks":
				if !agentReady {
					agentReady = true
					n.line(cBlue, "AGENT", "SENTINEL-1 (UAV, Moron) and TRIDENT-2 (maritime patrol, Comodoro Rivadavia) online, listening for tasks")
				}
			case "task status":
				if f["task"] == taskID {
					n.line(cBlue, "AGENT", "%s reports %s (status version %s)", f["asset"], f["status"], f["version"])
				}
			case "task accepted":
				n.line(cBlue, "AGENT", "%s accepts %s on %s, %s km away, departing", f["asset"], f["kind"], f["target"], f["distance_km"])
			case "mission complete":
				n.line(cBlue, "AGENT", "%s held the target in EO/IR range for %s", f["asset"], f["on_target"])
			}
		case f := <-chanOrNil(md):
			switch {
			case f["msg"] == "rx stats" && !announcedPicture && f["frames_adsbfi"] != "":
				announcedPicture = true
				n.line(cCyan, "RX", "live picture over AMBA: %s entities from ADS-B (adsb.fi) and AIS (AISStream), published to Lattice", f["entities"])
			case f["msg"] == "attack started":
				injectedAt = time.Now()
				n.line(cYellow, "RED TEAM", "ghost aircraft injected 8 NM from Moron Air Base (SADM): FL050, no callsign, no history")
			case f["msg"] == "anomaly" && f["entity"] == target && f["escalate"] == "true" && detectedAt.IsZero():
				detectedAt = time.Now()
				n.line(cMag, "TIER 1", "%s sev %s (%s), %.0f s after injection, zero LLM cost", f["type"], f["severity"], f["rule"], secs(injectedAt, detectedAt))
			case f["msg"] == "decision" && f["entity"] == target && decidedAt.IsZero():
				decidedAt = time.Now()
				n.line(cMag, "TIER 2", "OpenAI Decisions API: P(manipulation)=%s, %s -> %s (%s ms)", f["p"], f["category"], f["recommend"], f["latency_ms"])
				n.line(cDim, "", "         evidence stored in Lattice Objects: %s", f["evidence"])
			case f["msg"] == "c2" && f["entity"] == target && dispatchedAt.IsZero():
				dispatchedAt = time.Now()
				taskID = f["task"]
				n.line(cRed, "C2", "override mil_view.disposition = DISPOSITION_HOSTILE, track re-symbolised")
				n.line(cRed, "C2", "VisualId task %s dispatched to %s (autonomous mode)", short(taskID), f["asset"])
			}
		case <-tick.C:
			if agentReady && !mdtiStarted {
				mdtiStarted = true
				md, err = startChild(ctx, "mdti", exe("mdti"), []string{"-env", o.env, "-modules=rx,ing,tier1,tier2",
					"-redteam=" + o.scenario, "-decisions=" + filepath.Join(o.logs, "decisions.jsonl")},
					[]string{"AUTONOMY_LEVEL=autonomous"}, o.logs)
				if err != nil {
					return err
				}
				n.line(cCyan, "RX", "MDTI daemon started: rx -> ing -> tier1 -> tier2 -> c2")
			}
			if taskID == "" {
				continue
			}
			t, err := clients.Unary.Tasks.GetTask(ctx, &Lattice.GetTaskRequest{TaskID: taskID})
			if err == nil && t.Status != nil && t.Status.Status != nil {
				st := string(*t.Status.Status)
				if st != lastStatus {
					lastStatus = st
					n.line(cGreen, "LATTICE", "task %s is %s (read back by the demo driver)", short(taskID), st)
				}
				if st == "STATUS_DONE_OK" {
					doneAt = time.Now()
				} else if st == "STATUS_DONE_NOT_OK" {
					return fmt.Errorf("task ended %s", st)
				}
			}
			if time.Since(lastProgress) >= 15*time.Second && lastStatus == "STATUS_EXECUTING" {
				lastProgress = time.Now()
				if e, err := clients.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: "mdti-asset-sentinel-1"}); err == nil && e.Description != nil {
					n.line(cBlue, "AGENT", "%s", strings.TrimPrefix(*e.Description, "SENTINEL-1 · "))
				}
			}
		}
	}

	n.line("", "", "")
	if e, err := clients.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: target}); err == nil {
		disp, sidc, ov := "DISPOSITION_UNKNOWN", "", ""
		if e.MilView != nil && e.MilView.Disposition != nil {
			disp = string(*e.MilView.Disposition)
		}
		if e.Symbology != nil && e.Symbology.MilStd2525C != nil && e.Symbology.MilStd2525C.Sidc != nil {
			sidc = *e.Symbology.MilStd2525C.Sidc
		}
		if e.Overrides != nil && len(e.Overrides.Override) > 0 && e.Overrides.Override[0].Status != nil {
			ov = string(*e.Overrides.Override[0].Status)
		}
		n.line(cGreen, "LATTICE", "%s: %s, SIDC %s, override %s", target, disp, sidc, ov)
	}
	n.line(cGreen, "RESULT", "%sinjection -> detection %.0f s | -> decision %.0f s | -> task %.0f s | -> DONE_OK %.0f s%s",
		cBold, secs(injectedAt, detectedAt), secs(detectedAt, decidedAt), secs(decidedAt, dispatchedAt), secs(dispatchedAt, doneAt), cReset)

	if !o.keepOverride {
		if _, err := clients.Unary.Entities.RemoveEntityOverride(context.Background(), &Lattice.RemoveEntityOverrideRequest{EntityID: target, FieldPath: "mil_view.disposition"}); err == nil {
			n.line(cDim, "CLEANUP", "override removed; stopping the daemon and the agent")
		}
	}
	return nil
}

func chanOrNil(c *child) chan map[string]string {
	if c == nil {
		return nil
	}
	return c.lines
}

func secs(a, b time.Time) float64 {
	if a.IsZero() || b.IsZero() {
		return 0
	}
	return b.Sub(a).Seconds()
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
