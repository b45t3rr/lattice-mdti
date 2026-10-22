// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package agent

import (
	"fmt"
	"math"
	"time"

	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/geo"
	"github.com/b45t3rr/lattice-mdti/internal/lattice"
)

// Mode is the asset-level state, separate from task status (SPEC §8.5).
type Mode string

const (
	ModeIdle      Mode = "IDLE"
	ModeTransit   Mode = "TRANSIT"
	ModeOnStation Mode = "ON_STATION"
	ModeRTB       Mode = "RTB"
	ModePatrol    Mode = "PATROL" // orbiting its patrol station (demo profile)
)

// AssetConfig describes one simulated patrol asset.
type AssetConfig struct {
	ID           string
	Name         string
	PlatformType string
	Domain       domain.Domain // which targets it serves
	Home         geo.Point
	CruiseMps    float64
	AltHaeM      float64
	Endurance    time.Duration
	// Station, when set, is a patrol orbit the asset holds while idle and
	// returns to after a task (demo profile: assets already airborne).
	Station       *geo.Point
	StationRadius float64
}

// DefaultAssets are SENTINEL-1 at Morón and TRIDENT-2 at Comodoro Rivadavia
// (SPEC §8.2). Home and speed are kept in sync with data/tier2/policy.yaml.
func DefaultAssets() []AssetConfig {
	return []AssetConfig{
		{ID: "mdti-asset-sentinel-1", Name: "SENTINEL-1", PlatformType: "UAV", Domain: domain.DomainAir,
			Home: geo.Point{Lat: -34.6763, Lon: -58.6428}, CruiseMps: 45, AltHaeM: 1500, Endurance: 8 * time.Hour},
		{ID: "mdti-asset-sentinel-2", Name: "SENTINEL-2", PlatformType: "UAV", Domain: domain.DomainAir,
			Home: geo.Point{Lat: -34.8222, Lon: -58.5358}, CruiseMps: 45, AltHaeM: 1800, Endurance: 8 * time.Hour},
		{ID: "mdti-asset-trident-2", Name: "TRIDENT-2", PlatformType: "Maritime Patrol UAV", Domain: domain.DomainSea,
			Home: geo.Point{Lat: -45.7853, Lon: -67.4655}, CruiseMps: 110, AltHaeM: 3000, Endurance: 24 * time.Hour},
	}
}

// DemoProfile places assets on patrol stations so the demo does not wait for
// long transits: SENTINEL-2 on a CAP over Buenos Aires, TRIDENT-2 over the
// "Milla 201" fishing grounds. It is a labelled demo setting, not the default.
func DemoProfile(assets []AssetConfig) []AssetConfig {
	out := make([]AssetConfig, len(assets))
	copy(out, assets)
	for i := range out {
		switch out[i].ID {
		case "mdti-asset-sentinel-2":
			out[i].Station, out[i].StationRadius = &geo.Point{Lat: -34.70, Lon: -58.30}, 6000
		case "mdti-asset-trident-2":
			out[i].Station, out[i].StationRadius = &geo.Point{Lat: -46.30, Lon: -61.10}, 6000
		}
	}
	return out
}

// Supported task specifications (advertised in the task catalog).
var catalog = []string{
	"type.googleapis.com/anduril.tasks.v2.VisualId",
	"type.googleapis.com/anduril.tasks.v2.Investigate",
}

// entity renders the asset as a complete TEMPLATE_ASSET entity (SPEC §3.4.4).
func entity(cfg AssetConfig, s State, mode Mode, status string, online bool, integration string, now time.Time) *Lattice.Entity {
	disp := Lattice.MilViewDispositionDispositionFriendly
	env := Lattice.MilViewEnvironmentEnvironmentAir
	nat := Lattice.MilViewNationality("NATIONALITY_ARGENTINA")
	conn := Lattice.HealthConnectionStatusConnectionStatusOnline
	if !online {
		conn = Lattice.HealthConnectionStatusConnectionStatusOffline
	}
	health := Lattice.HealthHealthStatusHealthStatusHealthy
	camera, rf := Lattice.SensorSensorTypeSensorTypeCamera, Lattice.SensorSensorTypeSensorTypeRf
	var defs []*Lattice.TaskDefinition
	for _, u := range catalog {
		defs = append(defs, &Lattice.TaskDefinition{TaskSpecificationURL: Lattice.String(u)})
	}
	alt := cfg.AltHaeM
	if mode == ModeIdle {
		alt = 0
	}
	ve, vn := geo.VelocityENU(s.SpeedMps, s.Heading)
	yaw := (90 - s.Heading) * math.Pi / 180
	return &Lattice.Entity{
		EntityID:    Lattice.String(cfg.ID),
		Description: Lattice.String(fmt.Sprintf("%s · %s", cfg.Name, status)),
		IsLive:      Lattice.Bool(true),
		ExpiryTime:  ptr(now.Add(30 * time.Second)),
		Aliases:     &Lattice.Aliases{Name: Lattice.String(cfg.Name)},
		Ontology:    &Lattice.Ontology{Template: ptr(Lattice.OntologyTemplateTemplateAsset), PlatformType: Lattice.String(cfg.PlatformType)},
		MilView:     &Lattice.MilView{Disposition: &disp, Environment: &env, Nationality: &nat},
		Symbology: &Lattice.Symbology{MilStd2525C: &Lattice.MilStd2525C{
			Sidc: Lattice.String(lattice.SIDC(disp, env, lattice.FuncMilitaryUAV)),
		}},
		DataClassification: &Lattice.Classification{Default: &Lattice.ClassificationInformation{
			Level: ptr(Lattice.ClassificationInformationLevelClassificationLevelsUnclassified),
		}},
		Location: &Lattice.Location{
			Position:    &Lattice.Position{LatitudeDegrees: Lattice.Float64(s.Pos.Lat), LongitudeDegrees: Lattice.Float64(s.Pos.Lon), AltitudeHaeMeters: Lattice.Float64(alt)},
			VelocityEnu: &Lattice.Enu{E: Lattice.Float64(ve), N: Lattice.Float64(vn), U: Lattice.Float64(0)},
			SpeedMps:    Lattice.Float64(s.SpeedMps),
			AttitudeEnu: &Lattice.Quaternion{X: Lattice.Float64(0), Y: Lattice.Float64(0), Z: Lattice.Float64(math.Sin(yaw / 2)), W: Lattice.Float64(math.Cos(yaw / 2))},
		},
		Health: &Lattice.Health{HealthStatus: &health, ConnectionStatus: &conn, UpdateTime: &now},
		Sensors: &Lattice.Sensors{Sensors: []*Lattice.Sensor{
			{SensorID: Lattice.String("eo-ir"), SensorType: &camera, SensorDescription: Lattice.String("EO/IR turret")},
			{SensorID: Lattice.String("esm"), SensorType: &rf, SensorDescription: Lattice.String("ESM / RF direction finding")},
		}},
		TaskCatalog: &Lattice.TaskCatalog{TaskDefinitions: defs},
		Provenance: &Lattice.Provenance{
			IntegrationName: Lattice.String(integration + "-agent"), DataType: Lattice.String("asset-sim"),
			SourceUpdateTime: &now,
		},
	}
}

func ptr[T any](v T) *T { return &v }
