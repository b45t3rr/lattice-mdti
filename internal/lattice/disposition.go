// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package lattice

import (
	Lattice "github.com/anduril/lattice-sdk-go/v5"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Disposition maps MDTI's vocabulary to Lattice literals. Lattice has no
// "SUSPECT": it is DISPOSITION_SUSPICIOUS (SPEC §3.4.1).
func Disposition(d domain.Disposition) (Lattice.MilViewDisposition, bool) {
	switch d {
	case domain.DispositionFriendly:
		return Lattice.MilViewDispositionDispositionFriendly, true
	case domain.DispositionNeutral:
		return Lattice.MilViewDispositionDispositionNeutral, true
	case domain.DispositionSuspect:
		return Lattice.MilViewDispositionDispositionSuspicious, true
	case domain.DispositionHostile:
		return Lattice.MilViewDispositionDispositionHostile, true
	}
	return "", false
}
