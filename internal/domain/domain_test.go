// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFrameValidate(t *testing.T) {
	now := time.Now()
	ok := Frame{EntityKey: "adsb:e1724f", Domain: DomainAir, Lat: -34.6, Lon: -58.5, ObservedAt: now}
	assert.NoError(t, ok.Validate(now))

	tests := map[string]func(*Frame){
		"empty key":   func(f *Frame) { f.EntityKey = "" },
		"bad domain":  func(f *Frame) { f.Domain = "SPACE" },
		"lat range":   func(f *Frame) { f.Lat = 91 },
		"lon range":   func(f *Frame) { f.Lon = -181 },
		"null island": func(f *Frame) { f.Lat, f.Lon = 0, 0 },
		"no time":     func(f *Frame) { f.ObservedAt = time.Time{} },
		"future":      func(f *Frame) { f.ObservedAt = now.Add(10 * time.Second) },
		"stale":       func(f *Frame) { f.ObservedAt = now.Add(-3 * time.Minute) },
	}
	for name, mut := range tests {
		t.Run(name, func(t *testing.T) {
			f := ok
			mut(&f)
			assert.Error(t, f.Validate(now))
		})
	}
}

func TestStripClearsSource(t *testing.T) {
	f := Frame{Source: SourceRedTeam, EntityKey: "adsb:x"}
	assert.Empty(t, f.Strip().Source)
	assert.Equal(t, SourceRedTeam, f.Source, "original untouched")
}

func TestSanitizeIdent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ARG1234  ", "ARG1234"},
		{"  LU   YUAN\tYU ", "LU YUAN YU"},
		{"BAD" + string(rune(0x00)) + string(rune(0x07)) + "NAME" + string(rune(0x202E)), "BADNAME"}, // NUL, BEL, RIGHT-TO-LEFT OVERRIDE
		{strings.Repeat("A", 100), strings.Repeat("A", MaxIdentRunes)},
		{"ÑANDÚ", "ÑANDÚ"},
		{"", ""},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, SanitizeIdent(tc.in), "%q", tc.in)
	}
}
