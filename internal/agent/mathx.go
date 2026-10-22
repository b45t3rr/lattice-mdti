// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package agent

import "math"

func hypot(v [2]float64) float64 { return math.Hypot(v[0], v[1]) }

// atan2deg converts an ENU velocity (east, north) to a true bearing.
func atan2deg(east, north float64) float64 { return math.Atan2(east, north) * 180 / math.Pi }
