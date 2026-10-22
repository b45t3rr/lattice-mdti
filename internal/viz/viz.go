// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package viz builds the map layers MDTI draws in the Lattice C2: zones,
// hazard markers, intercept lines and sensor points of interest. They are
// TEMPLATE_GEO / TEMPLATE_SENSOR_POINT_OF_INTEREST entities, so they render
// on the common operating picture next to the tracks.
package viz

import (
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/geo"
)

// RGBA is a display colour (alpha in [0,1]).
type RGBA struct{ R, G, B, A float64 }

// Palette used across MDTI layers (matches Lattice's disposition colours).
var (
	Orange = RGBA{255, 153, 0, 1}
	Red    = RGBA{255, 59, 48, 1}
	Cyan   = RGBA{0, 200, 255, 1}
	Grey   = RGBA{150, 150, 150, 1}
	Green  = RGBA{52, 199, 89, 1}
	Yellow = RGBA{255, 214, 10, 1}
)

// WithAlpha returns c with a different alpha.
func (c RGBA) WithAlpha(a float64) RGBA { c.A = a; return c }

func (c RGBA) color() *Lattice.Color {
	return &Lattice.Color{Red: Lattice.Float64(c.R), Green: Lattice.Float64(c.G), Blue: Lattice.Float64(c.B),
		Alpha: Lattice.Float64(c.A)}
}

func pos(p geo.Point) *Lattice.Position {
	return &Lattice.Position{LatitudeDegrees: Lattice.Float64(p.Lat), LongitudeDegrees: Lattice.Float64(p.Lon)}
}

// Style describes how a geo layer is drawn.
type Style struct {
	Type Lattice.GeoDetailsType
	Line RGBA
	Fill *RGBA
}

// Layer is one geo entity.
type Layer struct {
	ID, Name, Description string
	Style                 Style
	Shape                 *Lattice.GeoShape
	Center                *geo.Point // required by Lattice for ellipses
}

// Entity renders a layer as a TEMPLATE_GEO entity that expires at expiry.
func (l Layer) Entity(integration string, now, expiry time.Time) *Lattice.Entity {
	tmpl := Lattice.OntologyTemplateTemplateGeo
	vd := &Lattice.GeoVisualDetails{LineColor: l.Style.Line.color()}
	if l.Style.Fill != nil {
		vd.FillColor = l.Style.Fill.color()
	}
	typ := l.Style.Type
	if typ == "" {
		typ = Lattice.GeoDetailsTypeGeoTypeGeneral
	}
	e := &Lattice.Entity{
		EntityID:    Lattice.String(l.ID),
		Description: Lattice.String(l.Description),
		IsLive:      Lattice.Bool(true),
		ExpiryTime:  &expiry,
		Aliases:     &Lattice.Aliases{Name: Lattice.String(l.Name)},
		Ontology:    &Lattice.Ontology{Template: &tmpl, PlatformType: Lattice.String("Geo")},
		GeoShape:    l.Shape,
		GeoDetails:  &Lattice.GeoDetails{Type: &typ, VisualDetails: vd},
		Provenance: &Lattice.Provenance{IntegrationName: Lattice.String(integration), DataType: Lattice.String("map-layer"),
			SourceUpdateTime: &now},
	}
	if l.Center != nil {
		e.Location = &Lattice.Location{Position: pos(*l.Center)}
	}
	return e
}

// Circle is an ellipse with equal axes.
func Circle(center geo.Point, radiusM float64) (*Lattice.GeoShape, *geo.Point) {
	c := center
	return &Lattice.GeoShape{Ellipse: &Lattice.GeoEllipse{
		SemiMajorAxisM: Lattice.Float64(radiusM), SemiMinorAxisM: Lattice.Float64(radiusM), OrientationD: Lattice.Float64(0),
	}}, &c
}

// CirclePolygon approximates a circle with a polygon (rendered everywhere
// polygons are, unlike ellipses on some views).
func CirclePolygon(center geo.Point, radiusM float64, n int) *Lattice.GeoShape {
	var pts []geo.Point
	for i := range n {
		pts = append(pts, geo.Destination(center, float64(i)*360/float64(n), radiusM))
	}
	return Polygon(pts)
}

// Polygon closes the ring if needed.
func Polygon(pts []geo.Point) *Lattice.GeoShape {
	if len(pts) > 0 && pts[0] != pts[len(pts)-1] {
		pts = append(pts, pts[0])
	}
	ring := &Lattice.LinearRing{}
	for _, p := range pts {
		ring.Positions = append(ring.Positions, &Lattice.GeoPolygonPosition{Position: pos(p)})
	}
	return &Lattice.GeoShape{Polygon: &Lattice.GeoPolygon{Rings: []*Lattice.LinearRing{ring}}}
}

// Line is a polyline.
func Line(pts []geo.Point) *Lattice.GeoShape {
	l := &Lattice.GeoLine{}
	for _, p := range pts {
		l.Positions = append(l.Positions, pos(p))
	}
	return &Lattice.GeoShape{Line: l}
}

// Point is a single geo point.
func Point(p geo.Point) *Lattice.GeoShape {
	return &Lattice.GeoShape{Point: &Lattice.GeoPoint{Position: pos(p)}}
}
