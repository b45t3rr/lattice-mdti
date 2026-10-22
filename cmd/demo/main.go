// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command demo runs the end-to-end demonstration (README "Demo"): four
// red-team inconsistencies (a ghost track, impossible flight physics, GNSS
// spoofing, AIS switched off at the 200 NM line) are injected at the data
// layer, and the narration follows each one through detection, decision and
// the map. It is detection only: C2 runs with -tasking=false, so no asset is
// ever dispatched. With -cast it records an asciicast v2 file that
// tools/cast2gif turns into the README GIF.
//
// With -offline it needs no credentials at all: it also starts bin/sim (the
// offline sandbox: simulated Lattice, Decisions stub with recorded answers,
// live map in the browser) and feeds MDTI synthetic background traffic.
//
// Build first (`make build`): the demo runs bin/mdti (and bin/sim offline).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/config"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

type options struct {
	env, bin, cast, logs, simAddr string
	timeout, hold                 time.Duration
	keepOverride, offline, open   bool
}

func main() {
	var o options
	flag.StringVar(&o.env, "env", ".env", "optional .env file")
	flag.StringVar(&o.bin, "bin", "bin", "directory with the mdti and sim executables")
	flag.StringVar(&o.cast, "cast", "", "record the narration to this asciicast v2 file")
	flag.StringVar(&o.logs, "logs", "demo-logs", "directory for the child processes' logs")
	flag.DurationVar(&o.timeout, "timeout", 20*time.Minute, "abort after this long")
	flag.BoolVar(&o.keepOverride, "keep-override", false, "leave the disposition overrides in Lattice afterwards")
	flag.BoolVar(&o.offline, "offline", false, "run against the local offline sandbox (bin/sim): no Lattice or OpenAI account needed")
	flag.StringVar(&o.simAddr, "sim-addr", "127.0.0.1:8740", "offline sandbox listen address (loopback)")
	flag.BoolVar(&o.open, "open", true, "offline: open the sandbox map in the browser")
	flag.DurationVar(&o.hold, "hold", 0, "keep everything running this long after the last act (to look at the map)")
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
		"title": "MDTI — four data-layer inconsistencies, detected and explained on the map",
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

// act is one storyline of the demo: a red-team scenario, the track it
// creates, and what the operator should look at on the map.
type act struct {
	num        int
	title      string
	scenario   string // scenario file
	name       string // scenario name (as logged by mdti)
	entity     string // Lattice entity of the threat
	where      string // where to look on the map
	inject     string // what the red team does
	injectedAt time.Time
	detectedAt time.Time
	decidedAt  time.Time
	markedAt   time.Time
}

// demoActs are numbered in the order they are detected.
func demoActs() []*act {
	return []*act{
		{num: 1, title: "Ghost aircraft near Moron Air Base", scenario: "scenarios/demo_ghost.yaml", name: "ghost_sadm_demo",
			entity: "mdti-adsb-e3f00d", where: "map: Moron (SADM), orange 15 NM watch ring",
			inject: "a track appears 8 NM from Moron Air Base at FL050: no callsign, no history, mid-coverage"},
		{num: 2, title: "Impossible flight physics", scenario: "scenarios/demo_kinematic.yaml", name: "kinematic_demo",
			entity: "mdti-adsb-e0c0de", where: "map: south of Ezeiza, airliner ARG1234",
			inject: "airliner ARG1234 flies normally; in 60 s its reported position jumps 12 NM sideways in 2 s"},
		{num: 3, title: "GPS spoofing over the Rio de la Plata", scenario: "scenarios/demo_gnss.yaml", name: "gnss_spoof_demo",
			entity: "mdti-adsb-e0babe", where: "map: Rio de la Plata, airliner ARG5678",
			inject: "airliner ARG5678 inbound over the river; in 60 s its GNSS altitude drifts away from baro and NACp/NIC collapse"},
		{num: 4, title: "AIS switched off at the Milla 201", scenario: "scenarios/demo_dark.yaml", name: "dark_vessel_demo",
			entity: "mdti-ais-200000001", where: "map: offshore Golfo San Jorge, cyan 200 NM line (~46S 61W)",
			inject: "fishing vessel cruising outside the 200 NM line; in 3 min it turns into the EEZ and switches AIS off"},
	}
}

func run(o options, n *narrator) error {
	// sigCtx bounds the child processes and the final hold; ctx adds the
	// timeout, which applies to the acts only.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(sigCtx, o.timeout)
	defer cancel()

	n.line("", "", "%sMDTI · Multi-Domain Airspace & Maritime Threat Intelligence on Anduril Lattice%s", cBold, cReset)
	if o.offline {
		n.line("", "", "%sDefensive R&D demo · OFFLINE SANDBOX · detection only · red team injects at the data layer (no RF)%s", cDim, cReset)
		n.line("", "", "%sOFFLINE MODE: simulated Lattice, Tier-2 stub replaying recorded model answers, synthetic background traffic%s", cYellow, cReset)
	} else {
		n.line("", "", "%sDefensive R&D demo · Lattice Sandbox · detection only · red team injects at the data layer (no RF)%s", cDim, cReset)
	}
	n.line("", "", "%sDEMO PROFILE: dark-vessel silence threshold 3 min (SPEC default 10 min); no assets are dispatched%s", cYellow, cReset)
	n.line("", "", "")

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
	if o.offline {
		sim, err := startSandbox(sigCtx, o, exe("sim"), n)
		if err != nil {
			return err
		}
		defer sim.stop()
	}

	cfg, err := config.Load(o.env)
	if err != nil {
		return err
	}
	clients, err := lattice.New(cfg.Lattice)
	if err != nil {
		return err
	}
	// Tolerate a brief network blip at start-up: retry for up to ~30 s.
	var pingErr error
	for attempt := range 6 {
		_, pingErr = clients.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: "mdti-demo-ping"})
		if pingErr == nil || lattice.StatusCode(pingErr) == 404 {
			pingErr = nil
			break
		}
		n.line(cYellow, "SETUP", "Lattice not reachable (attempt %d/6), retrying", attempt+1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if pingErr != nil {
		return fmt.Errorf("lattice: %s", lattice.Describe(pingErr))
	}
	if o.offline {
		n.line(cGreen, "SETUP", "offline sandbox reachable (%s)", cfg.Lattice.Endpoint)
	} else {
		n.line(cGreen, "SETUP", "Lattice Sandbox authenticated (%s)", lattice.RedactEndpoint(cfg.Lattice.Endpoint))
	}
	if cfg.OpenAI.APIKey.Reveal() == "" {
		return errors.New("OPENAI_API_KEY is required for Tier 2")
	}

	// Offline, say plainly what answers: the sandbox and a stub, not Lattice
	// and the model.
	platform, tier2API := "LATTICE", "Decisions API"
	if o.offline {
		platform, tier2API = "SANDBOX", "Decisions stub (recorded answer)"
	}

	acts := demoActs()
	byScenario, byEntity := map[string]*act{}, map[string]*act{}
	var scenarios []string
	for _, a := range acts {
		byScenario[a.name], byEntity[a.entity] = a, a
		scenarios = append(scenarios, a.scenario)
	}

	sources := "-sources=adsb,ais"
	if o.offline {
		sources = "-sources=synthetic"
	}
	// AUTONOMY_LEVEL=autonomous lets C2 apply dispositions without an
	// operator; -tasking=false keeps it from dispatching any asset.
	md, err := startChild(sigCtx, "mdti", exe("mdti"), []string{"-env", o.env, "-modules=rx,ing,tier1,tier2", sources, "-tasking=false",
		"-redteam=" + strings.Join(scenarios, ","), "-decisions=" + filepath.Join(o.logs, "decisions.jsonl")},
		[]string{"AUTONOMY_LEVEL=autonomous", "TIER1_DARK_MIN_GAP=3m"}, o.logs)
	if err != nil {
		return err
	}
	defer md.stop()
	n.line(cCyan, "RX", "MDTI daemon started: rx -> ing -> tier1 -> tier2 -> c2 (detection only), map overlays published")

	announcedPicture := false
	allMarked := func() bool {
		for _, a := range acts {
			if a.markedAt.IsZero() {
				return false
			}
		}
		return true
	}
	for !allMarked() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("demo did not complete: %w", ctx.Err())
		case <-md.done:
			return fmt.Errorf("mdti exited unexpectedly (see %s)", filepath.Join(o.logs, "mdti.log"))
		case f := <-md.lines:
			a := byEntity[f["entity"]]
			switch {
			case f["msg"] == "rx stats" && !announcedPicture && f["frames_adsbfi"] != "":
				announcedPicture = true
				n.line(cCyan, "RX", "live picture: %s entities from ADS-B (adsb.fi) and AIS (AISStream) published to Lattice", f["entities"])
			case f["msg"] == "rx stats" && !announcedPicture && f["frames_synthetic"] != "":
				announcedPicture = true
				n.line(cCyan, "RX", "background picture: %s synthetic aircraft and vessels published (Tier 1 should ignore all of them)", f["entities"])
			case f["msg"] == "attack started" && byScenario[f["scenario"]] != nil:
				a := byScenario[f["scenario"]]
				a.injectedAt = time.Now()
				n.line("", "", "")
				n.line(cBold, "", "%s== ACT %d · %s ==%s   %s(%s)%s", cBold, a.num, a.title, cReset, cDim, a.where, cReset)
				n.line(cYellow, "RED TEAM", "%s", a.inject)
			case f["msg"] == "anomaly" && f["escalate"] == "true" && a != nil && a.detectedAt.IsZero():
				a.detectedAt = time.Now()
				n.line(cMag, "TIER 1", "act %d: %s sev %s (%s), zero LLM cost", a.num, f["type"], f["severity"], f["rule"])
			case f["msg"] == "decision" && a != nil && a.decidedAt.IsZero():
				a.decidedAt = time.Now()
				n.line(cMag, "TIER 2", "act %d: %s: P(manipulation)=%s, %s -> %s (%s ms)", a.num, tier2API, f["p"], f["category"],
					nonEmpty(f["recommend"], "no change"), f["latency_ms"])
			case f["msg"] == "c2" && a != nil && a.markedAt.IsZero():
				a.markedAt = time.Now()
				if f["overridden"] != "true" {
					return fmt.Errorf("act %d: disposition was not applied (see %s)", a.num, filepath.Join(o.logs, "mdti.log"))
				}
				n.line(cRed, "C2", "act %d: track reclassified; evidence on the map: %s", a.num, nonEmpty(f["evidence"], "(alert drawn)"))
				if f["dead_reckoning"] == "true" {
					n.line(cRed, "C2", "act %d: AIS OFF marker + dead-reckoned track; vessel kept on the map at its estimated position", a.num)
				}
				n.line(cGreen, "RESULT", "%sact %d: injection -> detection %.0f s | -> decision %.0f s | -> on the map %.0f s%s",
					cBold, a.num, secs(a.injectedAt, a.detectedAt), secs(a.detectedAt, a.decidedAt), secs(a.decidedAt, a.markedAt), cReset)
			}
		}
	}

	n.line("", "", "")
	for _, a := range acts {
		if e, err := clients.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: a.entity}); err == nil {
			disp := "DISPOSITION_UNKNOWN"
			if e.MilView != nil && e.MilView.Disposition != nil {
				disp = string(*e.MilView.Disposition)
			}
			n.line(cGreen, platform, "act %d · %s: %s", a.num, a.entity, disp)
		} else {
			n.line(cDim, platform, "act %d · %s: no longer reported (expired)", a.num, a.entity)
		}
	}
	if o.hold > 0 {
		n.line(cDim, "HOLD", "all acts complete; keeping the picture up for %s before cleanup (Ctrl-C to skip)", o.hold)
		select {
		case <-sigCtx.Done():
		case <-time.After(o.hold):
		}
	}
	if !o.keepOverride {
		for _, a := range acts {
			_, _ = clients.Unary.Entities.RemoveEntityOverride(context.Background(), &Lattice.RemoveEntityOverrideRequest{EntityID: a.entity, FieldPath: "mil_view.disposition"})
		}
		n.line(cDim, "CLEANUP", "overrides removed; stopping the daemon (map layers expire on their own)")
	}
	return nil
}

// startSandbox launches bin/sim, waits until it listens and points this
// process and its children at it. Token values are placeholders: the
// sandbox has no authentication and listens on loopback only.
func startSandbox(ctx context.Context, o options, exe string, n *narrator) (*child, error) {
	base := "http://" + o.simAddr
	for k, v := range map[string]string{
		"LATTICE_ENDPOINT": base, "ENVIRONMENT_TOKEN": "offline", "SANDBOXES_TOKEN": "offline",
		"OPENAI_API_KEY": "offline", "OPENAI_BASE_URL": base + "/v1",
	} {
		if err := os.Setenv(k, v); err != nil {
			return nil, err
		}
	}
	sim, err := startChild(ctx, "sim", exe, []string{"-addr", o.simAddr}, nil, o.logs)
	if err != nil {
		return nil, err
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f := <-sim.lines:
			if f["msg"] != "sim listening" {
				continue
			}
			n.line(cGreen, "SETUP", "offline sandbox up · live map at %s%s/%s", cBold, base, cReset)
			if o.open {
				openBrowser(base + "/")
			}
			return sim, nil
		case <-sim.done:
			return nil, fmt.Errorf("sandbox exited (see %s): is %s already in use?", filepath.Join(o.logs, "sim.log"), o.simAddr)
		case <-deadline:
			sim.stop()
			return nil, errors.New("sandbox did not start within 10 s")
		case <-ctx.Done():
			sim.stop()
			return nil, ctx.Err()
		}
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

func secs(a, b time.Time) float64 {
	if a.IsZero() || b.IsZero() {
		return 0
	}
	return b.Sub(a).Seconds()
}
