// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package geo provides the spherical-earth primitives used by RX, Tier 1 and
// the agent (SPEC §6.1). Angles are degrees, distances metres.
package geo

import "math"

// EarthRadiusM is the IUGG mean Earth radius.
const EarthRadiusM = 6371008.8

// Point is a WGS-84 position in degrees.
type Point struct{ Lat, Lon float64 }

func rad(d float64) float64 { return d * math.Pi / 180 }
func deg(r float64) float64 { return r * 180 / math.Pi }

// Haversine returns the great-circle distance between a and b in metres.
func Haversine(a, b Point) float64 {
	φ1, φ2 := rad(a.Lat), rad(b.Lat)
	dφ, dλ := φ2-φ1, rad(b.Lon-a.Lon)
	h := math.Sin(dφ/2)*math.Sin(dφ/2) + math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	return 2 * EarthRadiusM * math.Atan2(math.Sqrt(h), math.Sqrt(1-h))
}

// Bearing returns the initial true bearing from a to b in [0,360).
func Bearing(a, b Point) float64 {
	φ1, φ2 := rad(a.Lat), rad(b.Lat)
	dλ := rad(b.Lon - a.Lon)
	y := math.Sin(dλ) * math.Cos(φ2)
	x := math.Cos(φ1)*math.Sin(φ2) - math.Sin(φ1)*math.Cos(φ2)*math.Cos(dλ)
	return NormalizeDeg(deg(math.Atan2(y, x)))
}

// Destination returns the point reached from p travelling distM metres on
// initial bearing bearingDeg along a great circle.
func Destination(p Point, bearingDeg, distM float64) Point {
	φ1, λ1, θ := rad(p.Lat), rad(p.Lon), rad(bearingDeg)
	δ := distM / EarthRadiusM
	φ2 := math.Asin(math.Sin(φ1)*math.Cos(δ) + math.Cos(φ1)*math.Sin(δ)*math.Cos(θ))
	λ2 := λ1 + math.Atan2(math.Sin(θ)*math.Sin(δ)*math.Cos(φ1), math.Cos(δ)-math.Sin(φ1)*math.Sin(φ2))
	return Point{Lat: deg(φ2), Lon: NormalizeLon(deg(λ2))}
}

// AngleDiff returns the signed smallest difference b−a in (−180,180].
func AngleDiff(a, b float64) float64 {
	d := math.Mod(b-a+540, 360) - 180
	if d == -180 {
		return 180
	}
	return d
}

// NormalizeDeg maps an angle into [0,360).
func NormalizeDeg(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}

// NormalizeLon maps a longitude into [−180,180).
func NormalizeLon(l float64) float64 { return NormalizeDeg(l+180) - 180 }

// ENU projects p onto a local tangent plane centred at origin (equirectangular,
// accurate to <0.5 % within ~500 km). Returns east, north in metres.
func ENU(origin, p Point) (east, north float64) {
	east = EarthRadiusM * rad(AngleDiff(origin.Lon, p.Lon)) * math.Cos(rad(origin.Lat))
	north = EarthRadiusM * rad(p.Lat-origin.Lat)
	return east, north
}

// VelocityENU decomposes a ground speed (m/s) on a true track into east/north.
func VelocityENU(speedMps, trackDeg float64) (east, north float64) {
	return speedMps * math.Sin(rad(trackDeg)), speedMps * math.Cos(rad(trackDeg))
}

// DeadReckon projects p along courseDeg at speedMps for dt (dt ≤ 0 keeps p).
func DeadReckon(p Point, courseDeg, speedMps float64, dtSeconds float64) Point {
	if dtSeconds <= 0 || speedMps <= 0 {
		return p
	}
	return Destination(p, courseDeg, speedMps*dtSeconds)
}
