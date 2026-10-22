// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Command validate is the separate-process live checker for the Lattice
// Sandbox (SPEC §10.2). It builds its own client from the environment, so a
// passing check proves the server-side state, not the daemon's view of it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/config"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

const usage = `usage: validate [-env .env] <command> [args]

commands:
  ping                       check credentials and connectivity
  get <entity-id>...         read entities back and print their key components
  watch [-prefix p] [-for d] stream live entities whose id starts with prefix and summarise them
  object <path>...           read Objects API objects back (e.g. Tier-2 evidence) and print them
  task <id> [-for d]         follow a task's status until it is terminal (separate-process lifecycle check)
  create-task -asset A -target E [-type vid|shadow]   dispatch a test task to an agent, then follow it
  cancel <id>                request cancellation of a task
  clear <entity-id>          remove MDTI's disposition override from an entity
  target [-lat -lon -for d]  publish a stationary test track (mdti-validate-target) for task checks
  viz-probe [-for d]         publish one example of every map element near Moron to check C2 rendering
  panel-probe [-for d]       publish six probe tracks to find which fields the C2 track panel shows
`

func main() {
	env := flag.String("env", ".env", "optional .env file")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	cfg, err := config.Load(*env)
	if err != nil {
		fail(err)
	}
	c, err := lattice.New(cfg.Lattice)
	if err != nil {
		fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := flag.Args()[1:]
	switch flag.Arg(0) {
	case "ping":
		err = ping(ctx, c)
	case "get":
		err = get(ctx, c, args)
	case "watch":
		err = watch(ctx, c, args)
	case "object":
		err = object(ctx, c, args)
	case "task":
		err = followTask(ctx, c, args)
	case "create-task":
		err = createTask(ctx, c, args)
	case "cancel":
		err = cancelTask(ctx, c, args)
	case "clear":
		err = clearOverride(ctx, c, args)
	case "target":
		err = publishTarget(ctx, c, args)
	case "panel-probe":
		err = panelProbe(ctx, c, args)
	case "viz-probe":
		err = vizProbe(ctx, c, args)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "validate:", lattice.Describe(err))
	os.Exit(1)
}

// ping reads an id that cannot exist: 404 proves auth and routing work.
func ping(ctx context.Context, c *lattice.Clients) error {
	_, err := c.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: "mdti-ping-does-not-exist"})
	switch code := lattice.StatusCode(err); {
	case err == nil, code == 404:
		fmt.Printf("OK: authenticated (%s) and reachable\n", c.AuthMode)
		return nil
	case code == 401 || code == 403:
		return fmt.Errorf("credentials rejected (HTTP %d): check ENVIRONMENT_TOKEN and SANDBOXES_TOKEN", code)
	default:
		return err
	}
}

func get(ctx context.Context, c *lattice.Clients, ids []string) error {
	if len(ids) == 0 {
		return errors.New("get: at least one entity id")
	}
	var failed int
	for _, id := range ids {
		e, err := c.Unary.Entities.GetEntity(ctx, &Lattice.GetEntityRequest{EntityID: id})
		if err != nil {
			failed++
			fmt.Printf("%s: %s\n", id, lattice.Describe(err))
			continue
		}
		fmt.Println(summary(e))
		raw, _ := json.MarshalIndent(e, "  ", "  ")
		fmt.Printf("  %s\n", raw)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d entities not readable", failed, len(ids))
	}
	return nil
}

func watch(ctx context.Context, c *lattice.Clients, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	prefix := fs.String("prefix", "mdti-", "entity id prefix")
	dur := fs.Duration("for", 30*time.Second, "how long to watch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()

	hb := 10_000
	stream, err := c.Stream.Entities.StreamEntities(ctx, &Lattice.EntityStreamRequest{HeartbeatIntervalMs: &hb})
	if err != nil {
		return err
	}
	defer stream.Close()

	latest := map[string]*Lattice.Entity{}
	events := map[string]int{}
	total := 0
	for {
		msg, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		if msg.Entity == nil || msg.Entity.Entity == nil {
			continue
		}
		total++
		e := msg.Entity.Entity
		id := deref(e.EntityID)
		if !strings.HasPrefix(id, *prefix) {
			continue
		}
		latest[id] = e
		if msg.Entity.EventType != nil {
			events[string(*msg.Entity.EventType)]++
		}
	}
	fmt.Printf("watched %s: %d entity events in the environment, %d distinct %q entities\n", *dur, total, len(latest), *prefix)
	keys := make([]string, 0, len(events))
	for k := range events {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-28s %d\n", k, events[k])
	}
	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Println(summary(latest[id]))
	}
	if len(latest) == 0 {
		return fmt.Errorf("no %q entities seen", *prefix)
	}
	return nil
}

func summary(e *Lattice.Entity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-26s", deref(e.EntityID))
	if e.Aliases != nil {
		fmt.Fprintf(&b, " name=%-12q", deref(e.Aliases.Name))
	}
	if e.MilView != nil {
		// DISPOSITION_UNKNOWN is the proto zero value, so the server omits it.
		disp := "DISPOSITION_UNKNOWN"
		if e.MilView.Disposition != nil {
			disp = string(*e.MilView.Disposition)
		}
		fmt.Fprintf(&b, " %s/%s", disp, enum(e.MilView.Environment))
		if e.MilView.Nationality != nil {
			fmt.Fprintf(&b, "/%s", *e.MilView.Nationality)
		}
	}
	if e.Symbology != nil && e.Symbology.MilStd2525C != nil {
		fmt.Fprintf(&b, " sidc=%s", deref(e.Symbology.MilStd2525C.Sidc))
	}
	if e.Location != nil && e.Location.Position != nil {
		p := e.Location.Position
		fmt.Fprintf(&b, " pos=%.4f,%.4f", deref(p.LatitudeDegrees), deref(p.LongitudeDegrees))
	}
	if e.ExpiryTime != nil {
		fmt.Fprintf(&b, " expires_in=%s", time.Until(*e.ExpiryTime).Round(time.Second))
	}
	if e.IsLive != nil {
		fmt.Fprintf(&b, " live=%t", *e.IsLive)
	}
	return b.String()
}

func enum[T ~string](p *T) string {
	if p == nil {
		return "-"
	}
	return string(*p)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func object(ctx context.Context, c *lattice.Clients, paths []string) error {
	if len(paths) == 0 {
		return errors.New("object: at least one object path")
	}
	for _, p := range paths {
		r, err := c.Unary.Objects.GetObject(ctx, &Lattice.GetObjectRequest{ObjectPath: p})
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		body, err := io.ReadAll(io.LimitReader(r, 4<<20))
		if err != nil {
			return err
		}
		fmt.Printf("%s (%d bytes)\n%s\n", p, len(body), body)
	}
	return nil
}

// followTask polls a task and prints every status transition until terminal.
func followTask(ctx context.Context, c *lattice.Clients, args []string) error {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	dur := fs.Duration("for", 20*time.Minute, "give up after this long")
	if len(args) == 0 {
		return errors.New("task: task id required")
	}
	id := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()
	var seen []string
	last := ""
	for {
		t, err := c.Unary.Tasks.GetTask(ctx, &Lattice.GetTaskRequest{TaskID: id})
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("task %s not terminal after %s (seen: %s)", id, *dur, strings.Join(seen, " → "))
			}
			return err
		}
		st, ver := "", 0
		if t.Status != nil && t.Status.Status != nil {
			st = string(*t.Status.Status)
		}
		if t.Version != nil && t.Version.StatusVersion != nil {
			ver = *t.Version.StatusVersion
		}
		if st != last {
			last = st
			seen = append(seen, st)
			msg := ""
			if t.Status.TaskError != nil && t.Status.TaskError.Message != nil {
				msg = " error=" + string(deref(t.Status.TaskError.Code)) + " " + *t.Status.TaskError.Message
			}
			fmt.Printf("%s  %-22s v%d%s\n", time.Now().Format("15:04:05"), st, ver, msg)
		}
		if st == "STATUS_DONE_OK" || st == "STATUS_DONE_NOT_OK" || st == "STATUS_REPLACED" {
			fmt.Printf("lifecycle: %s\n", strings.Join(seen, " → "))
			if st != "STATUS_DONE_OK" {
				return fmt.Errorf("task ended %s", st)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("task %s not terminal after %s (seen: %s)", id, *dur, strings.Join(seen, " → "))
		case <-time.After(2 * time.Second):
		}
	}
}

func createTask(ctx context.Context, c *lattice.Clients, args []string) error {
	fs := flag.NewFlagSet("create-task", flag.ContinueOnError)
	asset := fs.String("asset", "mdti-asset-sentinel-1", "assignee agent entity id")
	target := fs.String("target", "", "target entity id")
	typ := fs.String("type", "vid", "vid | shadow")
	follow := fs.Bool("follow", true, "follow the task until terminal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		return errors.New("create-task: -target required")
	}
	url := "type.googleapis.com/anduril.tasks.v2.VisualId"
	if *typ == "shadow" {
		url = "type.googleapis.com/anduril.tasks.v2.Investigate"
	}
	t, err := c.Unary.Tasks.CreateTask(ctx, &Lattice.TaskCreation{
		DisplayName: Lattice.String("MDTI validate " + *typ + " " + *target),
		Description: Lattice.String("[MDTI] separate-process lifecycle check"),
		Specification: &Lattice.GoogleProtobufAny{Type: Lattice.String(url),
			ExtraProperties: map[string]any{"objective": map[string]any{"entityId": *target}}},
		Author:              &Lattice.Principal{System: &Lattice.System{ServiceName: Lattice.String("mdti-validate")}},
		Relations:           &Lattice.Relations{Assignee: &Lattice.Principal{System: &Lattice.System{EntityID: asset}}},
		IsExecutedElsewhere: Lattice.Bool(false),
	})
	if err != nil {
		return err
	}
	id := deref(t.Version.TaskID)
	fmt.Printf("created task %s → %s\n", id, *asset)
	if !*follow {
		return nil
	}
	return followTask(ctx, c, []string{id})
}

func cancelTask(ctx context.Context, c *lattice.Clients, args []string) error {
	if len(args) == 0 {
		return errors.New("cancel: task id required")
	}
	_, err := c.Unary.Tasks.CancelTask(ctx, &Lattice.TaskCancellation{TaskID: args[0],
		Author: &Lattice.Principal{System: &Lattice.System{ServiceName: Lattice.String("mdti-validate")}}})
	if err == nil {
		fmt.Println("cancellation requested:", args[0])
	}
	return err
}

func clearOverride(ctx context.Context, c *lattice.Clients, args []string) error {
	if len(args) == 0 {
		return errors.New("clear: entity id required")
	}
	_, err := c.Unary.Entities.RemoveEntityOverride(ctx, &Lattice.RemoveEntityOverrideRequest{EntityID: args[0], FieldPath: "mil_view.disposition"})
	if err == nil {
		fmt.Println("override removed:", args[0])
	}
	return err
}

// publishTarget keeps a stationary test track alive so task lifecycles can be
// checked independently of live traffic.
func publishTarget(ctx context.Context, c *lattice.Clients, args []string) error {
	fs := flag.NewFlagSet("target", flag.ContinueOnError)
	lat := fs.Float64("lat", -34.645, "latitude")
	lon := fs.Float64("lon", -58.680, "longitude")
	dur := fs.Duration("for", 10*time.Minute, "how long to keep it alive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()
	id := "mdti-validate-target"
	created := time.Now().UTC()
	for {
		now := time.Now().UTC()
		disp, env := Lattice.MilViewDispositionDispositionUnknown, Lattice.MilViewEnvironmentEnvironmentAir
		tmpl := Lattice.OntologyTemplateTemplateTrack
		_, err := c.Unary.Entities.PublishEntity(ctx, &Lattice.Entity{
			EntityID: &id, IsLive: Lattice.Bool(true), CreatedTime: &created, ExpiryTime: Lattice.Time(now.Add(30 * time.Second)),
			Description: Lattice.String("MDTI validation target (stationary)"),
			Aliases:     &Lattice.Aliases{Name: Lattice.String("MDTI TEST TARGET")},
			Ontology:    &Lattice.Ontology{Template: &tmpl, PlatformType: Lattice.String("Aircraft")},
			MilView:     &Lattice.MilView{Disposition: &disp, Environment: &env},
			Location: &Lattice.Location{Position: &Lattice.Position{LatitudeDegrees: lat, LongitudeDegrees: lon,
				AltitudeHaeMeters: Lattice.Float64(600)}},
			Provenance: &Lattice.Provenance{IntegrationName: Lattice.String("mdti-validate"), DataType: Lattice.String("test-target"),
				SourceUpdateTime: &now},
		})
		if err != nil && ctx.Err() == nil {
			return err
		}
		select {
		case <-ctx.Done():
			fmt.Println("test target stopped (it expires 30 s after the last publish)")
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}
