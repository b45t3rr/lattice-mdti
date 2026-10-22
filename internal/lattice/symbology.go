// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package lattice

import (
	Lattice "github.com/anduril/lattice-sdk-go/v5"
)

// Function is the 6-character MIL-STD-2525C function ID (positions 5–10).
type Function string

const (
	FuncUnspecified      Function = "------"
	FuncCivilFixedWing   Function = "CF----"
	FuncCivilRotary      Function = "CH----"
	FuncMilitaryUAV      Function = "MFQ---"
	FuncNonMilitarySea   Function = "X-----"
	FuncFishingVessel    Function = "XF----"
	FuncNonMilitaryCargo Function = "XM----"
)

// SIDC builds a 15-character MIL-STD-2525C symbol ID (SPEC §5.2):
// S + affiliation + battle dimension + P(resent) + function + "-----".
func SIDC(d Lattice.MilViewDisposition, e Lattice.MilViewEnvironment, f Function) string {
	if len(f) != 6 {
		f = FuncUnspecified
	}
	return "S" + string(affiliation(d)) + string(dimension(e)) + "P" + string(f) + "-----"
}

func affiliation(d Lattice.MilViewDisposition) byte {
	switch d {
	case Lattice.MilViewDispositionDispositionFriendly, Lattice.MilViewDispositionDispositionAssumedFriendly:
		return 'F'
	case Lattice.MilViewDispositionDispositionHostile:
		return 'H'
	case Lattice.MilViewDispositionDispositionSuspicious:
		return 'S'
	case Lattice.MilViewDispositionDispositionNeutral:
		return 'N'
	default:
		return 'U'
	}
}

func dimension(e Lattice.MilViewEnvironment) byte {
	switch e {
	case Lattice.MilViewEnvironmentEnvironmentAir:
		return 'A'
	case Lattice.MilViewEnvironmentEnvironmentSurface:
		return 'S'
	case Lattice.MilViewEnvironmentEnvironmentSubSurface:
		return 'U'
	case Lattice.MilViewEnvironmentEnvironmentLand:
		return 'G'
	case Lattice.MilViewEnvironmentEnvironmentSpace:
		return 'P'
	default:
		return 'Z'
	}
}

// WithAffiliation returns sidc with its affiliation (position 2) set for d.
func WithAffiliation(sidc string, d Lattice.MilViewDisposition) string {
	if len(sidc) != 15 {
		return sidc
	}
	b := []byte(sidc)
	b[1] = affiliation(d)
	return string(b)
}
