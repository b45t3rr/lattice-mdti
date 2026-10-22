// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package redteam

import (
	"math/rand/v2"
	"os"
)

func newRNG(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, 0)) }

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o644) }
