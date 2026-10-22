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

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
	"github.com/b45t3rr/lattice-mdti/internal/viz"
)

// vizProbe publishes one example of every map element MDTI could draw, near
// Morón, so their rendering in the Lattice C2 can be checked on screen.
func vizProbe(ctx context.Context, c *lattice.Clients, args []string) error {
	fs := flag.NewFlagSet("viz-probe", flag.ContinueOnError)
	dur := fs.Duration("for", 20*time.Minute, "how long to keep the probe entities alive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()

	sadm := geo.Point{Lat: -34.6763, Lon: -58.6428}
	saez := geo.Point{Lat: -34.8222, Lon: -58.5358}
	sabe := geo.Point{Lat: -34.5592, Lon: -58.4156}
	target := geo.Destination(sadm, 300, 8*domain.NMToM)
	uav := geo.Destination(target, 120, 4000)

	zones, err := geo.LoadZones("data/geo/zones.yaml")
	if err != nil {
		return err
	}
	var mile200 []geo.Point
	for _, l := range zones.OuterPolylines(400) {
		if l.LineID == zones.PrimaryLineID && len(l.Points) > len(mile200) {
			mile200 = l.Points
		}
	}

	// Upload the simulated EO/IR frame once.
	imgPath := "mdti/media/probe-eoir.png"
	if _, err := c.Unary.Objects.UploadObject(ctx, imgPath, bytes.NewReader(viz.EOIRSnapshot(
		[]string{"SENTINEL-1  VID  mdti-probe-target", "NO AIRCRAFT AT REPORTED POSITION", "GHOST CONFIRMED"}, 1))); err != nil {
		return fmt.Errorf("upload image: %w", err)
	}
	fmt.Println("uploaded", imgPath)

	ellipse, center := viz.Circle(sadm, 15*domain.NMToM)
	fill := viz.Orange.WithAlpha(0.08)
	redFill := viz.Red.WithAlpha(0.25)
	cyanFill := viz.Cyan.WithAlpha(0.10)
	hz, hzCenter := viz.Circle(target, 2*domain.NMToM)
	layers := []viz.Layer{
		{ID: "mdti-probe-zone-ellipse", Name: "PROBE 1 · SADM watch zone (ellipse, 15 NM)", Shape: ellipse, Center: center,
			Style: viz.Style{Line: viz.Orange, Fill: &fill}},
		{ID: "mdti-probe-zone-polygon", Name: "PROBE 2 · SAEZ control area (polygon)", Shape: viz.CirclePolygon(saez, 8000, 48),
			Style: viz.Style{Type: Lattice.GeoDetailsTypeGeoTypeControlArea, Line: viz.Cyan, Fill: &cyanFill}},
		{ID: "mdti-probe-line", Name: "PROBE 3 · line SADM → SABE", Shape: viz.Line([]geo.Point{sadm, sabe}),
			Style: viz.Style{Line: viz.Red}},
		{ID: "mdti-probe-hazard-point", Name: "PROBE 4 · hazard point", Shape: viz.Point(geo.Destination(target, 0, 3000)),
			Style: viz.Style{Type: Lattice.GeoDetailsTypeGeoTypeHazard, Line: viz.Red}},
		{ID: "mdti-probe-hazard-area", Name: "PROBE 5 · hazard area (ellipse 2 NM)", Shape: hz, Center: hzCenter,
			Style: viz.Style{Type: Lattice.GeoDetailsTypeGeoTypeHazard, Line: viz.Red, Fill: &redFill}},
		{ID: "mdti-probe-mile200", Name: "PROBE 6 · EEZ 200 NM limit (polyline)", Shape: viz.Line(mile200),
			Style: viz.Style{Line: viz.Cyan}},
	}

	disp := Lattice.MilViewDispositionDispositionHostile
	env := Lattice.MilViewEnvironmentEnvironmentAir
	friendly := Lattice.MilViewDispositionDispositionFriendly
	tTrack, tAsset := Lattice.OntologyTemplateTemplateTrack, Lattice.OntologyTemplateTemplateAsset
	tPOI := Lattice.OntologyTemplateTemplateSensorPointOfInterest
	camera := Lattice.SensorSensorTypeSensorTypeCamera
	track := Lattice.FieldOfViewModeSensorModeTrack
	img := Lattice.MediaItemTypeMediaTypeImage
	foot := func(p geo.Point, brg float64) *Lattice.Position {
		q := geo.Destination(p, brg, 700)
		return &Lattice.Position{LatitudeDegrees: Lattice.Float64(q.Lat), LongitudeDegrees: Lattice.Float64(q.Lon)}
	}

	for {
		now := time.Now().UTC()
		exp := now.Add(2 * time.Minute)
		prov := func(dt string) *Lattice.Provenance {
			return &Lattice.Provenance{IntegrationName: Lattice.String("mdti-probe"), DataType: Lattice.String(dt), SourceUpdateTime: &now}
		}
		ents := []*Lattice.Entity{}
		for _, l := range layers {
			ents = append(ents, l.Entity("mdti-probe", now, exp))
		}
		ents = append(ents,
			&Lattice.Entity{EntityID: Lattice.String("mdti-probe-target"), IsLive: Lattice.Bool(true), ExpiryTime: &exp,
				Aliases:  &Lattice.Aliases{Name: Lattice.String("PROBE 7 · hostile target")},
				Ontology: &Lattice.Ontology{Template: &tTrack, PlatformType: Lattice.String("Aircraft")},
				MilView:  &Lattice.MilView{Disposition: &disp, Environment: &env},
				Location: &Lattice.Location{Position: &Lattice.Position{LatitudeDegrees: Lattice.Float64(target.Lat),
					LongitudeDegrees: Lattice.Float64(target.Lon), AltitudeHaeMeters: Lattice.Float64(1500)}},
				Symbology:  &Lattice.Symbology{MilStd2525C: &Lattice.MilStd2525C{Sidc: Lattice.String("SHAPCF---------")}},
				Provenance: prov("probe-track")},
			&Lattice.Entity{EntityID: Lattice.String("mdti-probe-uav"), IsLive: Lattice.Bool(true), ExpiryTime: &exp,
				Aliases:  &Lattice.Aliases{Name: Lattice.String("PROBE 8 · UAV (FOV + ActiveTarget)")},
				Ontology: &Lattice.Ontology{Template: &tAsset, PlatformType: Lattice.String("UAV")},
				MilView:  &Lattice.MilView{Disposition: &friendly, Environment: &env},
				Location: &Lattice.Location{Position: &Lattice.Position{LatitudeDegrees: Lattice.Float64(uav.Lat),
					LongitudeDegrees: Lattice.Float64(uav.Lon), AltitudeHaeMeters: Lattice.Float64(1500)}},
				Symbology: &Lattice.Symbology{MilStd2525C: &Lattice.MilStd2525C{Sidc: Lattice.String("SFAPMFQ--------")}},
				Sensors: &Lattice.Sensors{Sensors: []*Lattice.Sensor{{SensorID: Lattice.String("eo-ir"), SensorType: &camera,
					FieldsOfView: []*Lattice.FieldOfView{{FovID: Lattice.Int(1), Mode: &track, Range: Lattice.Float64(5000),
						HorizontalFov: Lattice.Float64(0.2), VerticalFov: Lattice.Float64(0.15),
						ProjectedCenterRay: &Lattice.Position{LatitudeDegrees: Lattice.Float64(target.Lat), LongitudeDegrees: Lattice.Float64(target.Lon)},
						ProjectedFrustum: &Lattice.ProjectedFrustum{UpperLeft: foot(target, 315), UpperRight: foot(target, 45),
							BottomRight: foot(target, 135), BottomLeft: foot(target, 225)}}}}}},
				Relationships: &Lattice.Relationships{Relationships: []*Lattice.Relationship{{
					RelatedEntityID: Lattice.String("mdti-probe-target"), RelationshipID: Lattice.String("probe-active-target"),
					RelationshipType: &Lattice.RelationshipType{ActiveTarget: &Lattice.ActiveTarget{}}}}},
				Provenance: prov("probe-asset")},
			&Lattice.Entity{EntityID: Lattice.String("mdti-probe-poi"), IsLive: Lattice.Bool(true), ExpiryTime: &exp,
				Aliases:  &Lattice.Aliases{Name: Lattice.String("PROBE 9 · EO/IR capture (image)")},
				Ontology: &Lattice.Ontology{Template: &tPOI, PlatformType: Lattice.String("EO/IR capture")},
				MilView:  &Lattice.MilView{Disposition: &disp, Environment: &env},
				Location: &Lattice.Location{Position: &Lattice.Position{LatitudeDegrees: Lattice.Float64(target.Lat - 0.01),
					LongitudeDegrees: Lattice.Float64(target.Lon)}},
				Media: &Lattice.Media{Media: []*Lattice.MediaItem{{ItemIdentifier: Lattice.String("probe-eoir"), Type: &img,
					RelativePath: Lattice.String(imgPath)}}},
				Provenance: prov("probe-poi")},
		)
		ok := 0
		for _, e := range ents {
			if _, err := c.Unary.Entities.PublishEntity(ctx, e); err != nil {
				if ctx.Err() != nil {
					break
				}
				fmt.Printf("  %-28s REJECTED: %s\n", *e.EntityID, lattice.Describe(err))
				continue
			}
			ok++
		}
		fmt.Printf("%s published %d/%d probe entities\n", time.Now().Format("15:04:05"), ok, len(ents))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(20 * time.Second):
		}
	}
}
