// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package sim

import (
	_ "embed"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
)

//go:embed map.html
var mapHTML []byte

func (s *Server) mapPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(mapHTML)
}

// MapEntity is the flattened view of an entity the map draws.
type MapEntity struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Kind        string       `json:"kind"` // track | asset | geo | poi
	Env         string       `json:"env,omitempty"`
	Disposition string       `json:"disp,omitempty"`
	Overridden  bool         `json:"overridden,omitempty"`
	Lat         *float64     `json:"lat,omitempty"`
	Lon         *float64     `json:"lon,omitempty"`
	AltM        *float64     `json:"alt_m,omitempty"`
	HeadingDeg  *float64     `json:"heading,omitempty"`
	SpeedMps    *float64     `json:"speed,omitempty"`
	Platform    string       `json:"platform,omitempty"`
	Description string       `json:"desc,omitempty"`
	DataType    string       `json:"data_type,omitempty"`
	Shape       string       `json:"shape,omitempty"` // point | line | polygon | ellipse
	Points      [][2]float64 `json:"pts,omitempty"`
	RadiusM     float64      `json:"radius_m,omitempty"`
	Color       string       `json:"color,omitempty"`
	Hazard      bool         `json:"hazard,omitempty"`
	Live        bool         `json:"live"`
}

// MapTask is the flattened view of a task.
type MapTask struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Assignee string    `json:"assignee"`
	Target   string    `json:"target,omitempty"`
	Status   string    `json:"status"`
	Version  int       `json:"version"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// State is the whole picture served at /sim/state.
type State struct {
	Now      time.Time   `json:"now"`
	Entities []MapEntity `json:"entities"`
	Tasks    []MapTask   `json:"tasks"`
	Events   []Event     `json:"events"`
}

// Snapshot returns the current picture.
func (s *Server) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Now: s.Now().UTC(), Entities: []MapEntity{}, Tasks: []MapTask{}, Events: append([]Event{}, s.events...)}
	for id := range s.entities {
		if e := s.view(id); e != nil {
			me := flatten(e)
			_, me.Overridden = s.overrides[id]
			st.Entities = append(st.Entities, me)
		}
	}
	sort.Slice(st.Entities, func(i, j int) bool { return st.Entities[i].ID < st.Entities[j].ID })
	for _, t := range s.tasks {
		mt := MapTask{ID: deref(t.Version.TaskID), Name: deref(t.DisplayName), Assignee: assigneeID(t), Target: targetOf(t),
			Status: string(status(t)), Version: deref(t.Version.StatusVersion), Created: deref(t.CreateTime), Updated: deref(t.LastUpdateTime)}
		if t.Status != nil && t.Status.TaskError != nil {
			mt.Error = deref(t.Status.TaskError.Message)
		}
		st.Tasks = append(st.Tasks, mt)
	}
	sort.Slice(st.Tasks, func(i, j int) bool { return st.Tasks[i].Created.After(st.Tasks[j].Created) })
	return st
}

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	reply(w, s.Snapshot())
}

func flatten(e *Lattice.Entity) MapEntity {
	me := MapEntity{ID: deref(e.EntityID), Name: name(e), Description: deref(e.Description), Live: deref(e.IsLive)}
	if e.Provenance != nil {
		me.DataType = deref(e.Provenance.DataType)
	}
	if o := e.Ontology; o != nil {
		me.Platform = deref(o.PlatformType)
		switch deref(o.Template) {
		case Lattice.OntologyTemplateTemplateAsset:
			me.Kind = "asset"
		case Lattice.OntologyTemplateTemplateGeo:
			me.Kind = "geo"
		case Lattice.OntologyTemplateTemplateSensorPointOfInterest:
			me.Kind = "poi"
		default:
			me.Kind = "track"
		}
	}
	if mv := e.MilView; mv != nil {
		me.Disposition = strings.TrimPrefix(string(deref(mv.Disposition)), "DISPOSITION_")
		me.Env = strings.TrimPrefix(string(deref(mv.Environment)), "ENVIRONMENT_")
	}
	if me.Disposition == "" {
		me.Disposition = "UNKNOWN"
	}
	if l := e.Location; l != nil {
		if p := l.Position; p != nil && p.LatitudeDegrees != nil && p.LongitudeDegrees != nil {
			me.Lat, me.Lon, me.AltM = p.LatitudeDegrees, p.LongitudeDegrees, p.AltitudeHaeMeters
		}
		if v := l.VelocityEnu; v != nil && v.E != nil && v.N != nil && math.Hypot(*v.E, *v.N) > 0.1 {
			h := math.Mod(math.Atan2(*v.E, *v.N)*180/math.Pi+360, 360)
			sp := math.Hypot(*v.E, *v.N)
			me.HeadingDeg, me.SpeedMps = &h, &sp
		}
		if l.SpeedMps != nil {
			me.SpeedMps = l.SpeedMps
		}
	}
	if g := e.GeoShape; g != nil {
		pt := func(p *Lattice.Position) [2]float64 {
			return [2]float64{deref(p.LatitudeDegrees), deref(p.LongitudeDegrees)}
		}
		switch {
		case g.Point != nil && g.Point.Position != nil:
			me.Shape, me.Points = "point", [][2]float64{pt(g.Point.Position)}
		case g.Line != nil:
			me.Shape = "line"
			for _, p := range g.Line.Positions {
				me.Points = append(me.Points, pt(p))
			}
		case g.Polygon != nil && len(g.Polygon.Rings) > 0:
			me.Shape = "polygon"
			for _, p := range g.Polygon.Rings[0].Positions {
				if p.Position != nil {
					me.Points = append(me.Points, pt(p.Position))
				}
			}
		case g.Ellipse != nil && me.Lat != nil:
			me.Shape, me.RadiusM = "ellipse", deref(g.Ellipse.SemiMajorAxisM)
			me.Points = [][2]float64{{*me.Lat, *me.Lon}}
		}
	}
	if gd := e.GeoDetails; gd != nil {
		me.Hazard = deref(gd.Type) == Lattice.GeoDetailsTypeGeoTypeHazard
		if vd := gd.VisualDetails; vd != nil && vd.LineColor != nil {
			c := vd.LineColor
			me.Color = fmt.Sprintf("rgba(%.0f,%.0f,%.0f,%.2f)", deref(c.Red), deref(c.Green), deref(c.Blue), alpha(c.Alpha))
		}
	}
	return me
}

func alpha(a *float64) float64 {
	if a == nil {
		return 1
	}
	return *a
}
