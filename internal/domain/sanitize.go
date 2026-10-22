// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package domain

import (
	"strings"
	"unicode"
)

// MaxIdentRunes caps attacker-controlled identifiers (callsign, ship name, …).
const MaxIdentRunes = 32

// SanitizeIdent trims, drops control/non-printable runes, collapses inner
// whitespace and caps length. Applied at the RX boundary to every untrusted
// string (SPEC §7.7).
func SanitizeIdent(s string) string {
	var b strings.Builder
	n, space := 0, false
	for _, r := range strings.TrimSpace(s) {
		if n >= MaxIdentRunes {
			break
		}
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case !unicode.IsPrint(r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
			n++
		}
		space = false
		b.WriteRune(r)
		n++
	}
	return b.String()
}
