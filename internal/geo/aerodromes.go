// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package geo

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// Aerodrome is a reference point used by Tier 1 and the asset simulator.
type Aerodrome struct {
	ICAO   string  `json:"icao"`
	Name   string  `json:"name"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	ElevFt float64 `json:"elev_ft"`
}

func (a Aerodrome) Point() Point { return Point{Lat: a.Lat, Lon: a.Lon} }

// Aerodromes is an ICAO-keyed set.
type Aerodromes map[string]Aerodrome

// LoadAerodromes reads data/geo/aerodromes.json.
func LoadAerodromes(path string) (Aerodromes, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []Aerodrome
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(Aerodromes, len(list))
	for _, a := range list {
		out[a.ICAO] = a
	}
	return out, nil
}

// Nearest returns the closest aerodrome to p and its distance in metres.
func (as Aerodromes) Nearest(p Point) (Aerodrome, float64) {
	var best Aerodrome
	bestD := math.Inf(1)
	for _, a := range as {
		if d := Haversine(p, a.Point()); d < bestD {
			best, bestD = a, d
		}
	}
	return best, bestD
}
