// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
	"github.com/b45t3rr/lattice-mdti/internal/viz"
)

// panelProbe publishes six probe tracks north-west of Morón, each carrying ONE
// group of candidate fields, to find which of them the Lattice C2 shows in
// the track panel (where MDTI could put the evidence of an inconsistency).
func panelProbe(ctx context.Context, c *lattice.Clients, args []string) error {
	fs := flag.NewFlagSet("panel-probe", flag.ContinueOnError)
	dur := fs.Duration("for", 20*time.Minute, "how long to keep the probe tracks alive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()

	// A chart of the kind MDTI would attach as evidence.
	var baro, gnss []float64
	for i := range 60 {
		baro = append(baro, 24000)
		g := 24375.0
		if i >= 30 {
			g += float64(i-30) * 80
		}
		gnss = append(gnss, g)
	}
	imgPath := "mdti/media/probe-chart.png"
	chart := viz.LineChart("GNSS vs baro altitude (ft)", "last 60 s", "ft", []viz.Series{
		{Name: "baro", Color: viz.Cyan, Y: baro}, {Name: "GNSS", Color: viz.Red, Y: gnss}})
	if _, err := c.Unary.Objects.UploadObject(ctx, imgPath, bytes.NewReader(chart)); err != nil {
		return fmt.Errorf("upload chart: %w", err)
	}
	fmt.Println("uploaded", imgPath)

	origin := geo.Point{Lat: -34.42, Lon: -58.98} // NW of Morón, clear of the demo
	desc := "[MDTI] DESCRIPTION PROBE · IMPOSSIBLE KINEMATICS · 22.2 km in 3 s · 7,443 m/s (Mach 22). " +
		"If you can read this sentence in the track panel, MDTI can put its evidence here."
	img := Lattice.MediaItemTypeMediaTypeImage
	warn := Lattice.AlertLevelAlertLevelWarning
	ring := viz.Red.WithAlpha(1)

	type probe struct {
		letter, what string
		apply        func(e *Lattice.Entity, now time.Time)
	}
	probes := []probe{
		{"A", "Status (activity / role)", func(e *Lattice.Entity, _ time.Time) {
			e.Status = &Lattice.Status{PlatformActivity: Lattice.String("IMPOSSIBLE KINEMATICS · Mach 22"),
				Role: Lattice.String("MDTI: spoofed track")}
		}},
		{"B", "Indicators (simulated, exercise)", func(e *Lattice.Entity, _ time.Time) {
			e.Indicators = &Lattice.Indicators{Simulated: Lattice.Bool(true), Exercise: Lattice.Bool(true)}
		}},
		{"C", "TargetPriority (threat, HVT)", func(e *Lattice.Entity, _ time.Time) {
			e.TargetPriority = &Lattice.TargetPriority{Threat: &Lattice.Threat{IsThreat: Lattice.Bool(true)},
				HighValueTarget: &Lattice.HighValueTarget{IsHighValueTarget: Lattice.Bool(true), TargetPriority: Lattice.Int(1)}}
		}},
		{"D", "Media (evidence chart PNG)", func(e *Lattice.Entity, _ time.Time) {
			e.Media = &Lattice.Media{Media: []*Lattice.MediaItem{{ItemIdentifier: Lattice.String("mdti-evidence-chart"),
				Type: &img, RelativePath: Lattice.String(imgPath)}}}
		}},
		{"E", "Health.ActiveAlerts", func(e *Lattice.Entity, now time.Time) {
			e.Health = &Lattice.Health{UpdateTime: &now, ActiveAlerts: []*Lattice.Alert{{
				AlertCode: Lattice.String("MDTI-KINEMATIC"), Level: &warn, ActivatedTime: &now,
				Description: Lattice.String("IMPOSSIBLE KINEMATICS · 22.2 km in 3 s · Mach 22"),
				ActiveConditions: []*Lattice.AlertCondition{{ConditionCode: Lattice.String("D1.R2"),
					Description: Lattice.String("teleport: jump exceeds the airframe envelope")}}}}}
		}},
		{"F", "VisualDetails.RangeRings", func(e *Lattice.Entity, _ time.Time) {
			e.VisualDetails = &Lattice.VisualDetails{RangeRings: &Lattice.RangeRings{MinDistanceM: Lattice.Float64(1000),
				MaxDistanceM: Lattice.Float64(4000), RingCount: Lattice.Int(2), RingLineColor: &Lattice.Color{
					Red: Lattice.Float64(ring.R), Green: Lattice.Float64(ring.G), Blue: Lattice.Float64(ring.B), Alpha: Lattice.Float64(ring.A)}}}
		}},
	}

	disp := Lattice.MilViewDispositionDispositionSuspicious
	env := Lattice.MilViewEnvironmentEnvironmentAir
	tmpl := Lattice.OntologyTemplateTemplateTrack
	for {
		now := time.Now().UTC()
		exp := now.Add(2 * time.Minute)
		ok := 0
		for i, p := range probes {
			at := geo.Destination(origin, 90, float64(i)*5000)
			e := &Lattice.Entity{
				EntityID: Lattice.String("mdti-probe-panel-" + p.letter), IsLive: Lattice.Bool(true), ExpiryTime: &exp,
				Description: Lattice.String(desc),
				Aliases:     &Lattice.Aliases{Name: Lattice.String(fmt.Sprintf("PROBE %s · %s", p.letter, p.what))},
				Ontology:    &Lattice.Ontology{Template: &tmpl, PlatformType: Lattice.String("Aircraft")},
				MilView:     &Lattice.MilView{Disposition: &disp, Environment: &env},
				Location: &Lattice.Location{Position: &Lattice.Position{LatitudeDegrees: Lattice.Float64(at.Lat),
					LongitudeDegrees: Lattice.Float64(at.Lon), AltitudeHaeMeters: Lattice.Float64(3000)},
					VelocityEnu: &Lattice.Enu{E: Lattice.Float64(0), N: Lattice.Float64(1), U: Lattice.Float64(0)}},
				Symbology: &Lattice.Symbology{MilStd2525C: &Lattice.MilStd2525C{Sidc: Lattice.String("SSAPCF---------")}},
				Provenance: &Lattice.Provenance{IntegrationName: Lattice.String("mdti-probe"), DataType: Lattice.String("panel-probe"),
					SourceUpdateTime: &now},
			}
			p.apply(e, now)
			if _, err := c.Unary.Entities.PublishEntity(ctx, e); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				fmt.Printf("  PROBE %s %-34s REJECTED: %s\n", p.letter, p.what, lattice.Describe(err))
				continue
			}
			ok++
		}
		fmt.Printf("%s published %d/%d panel probes (%.4f,%.4f)\n", time.Now().Format("15:04:05"), ok, len(probes),
			origin.Lat, origin.Lon)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(20 * time.Second):
		}
	}
}
