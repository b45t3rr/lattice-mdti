// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package sources

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	got, err := Parse(" readsb:http://192.168.1.50:8080 , sbs:192.168.1.50:30003,nmea-udp::10110,nmea-tcp:pi.local:10110,ais,adsb,synthetic,")
	require.NoError(t, err)
	assert.Equal(t, []Spec{
		{Readsb, "http://192.168.1.50:8080"}, {SBS, "192.168.1.50:30003"}, {NMEAUDP, ":10110"},
		{NMEATCP, "pi.local:10110"}, {AISStream, ""}, {ADSB, ""}, {Synthetic, ""},
	}, got)

	none, err := Parse("")
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestParseRejects(t *testing.T) {
	for _, bad := range []string{
		"radar", "readsb", "readsb:192.168.1.50:8080", "sbs", "sbs:host", "nmea-udp:10110",
		"adsb:extra", "sbs:h:30003,sbs:h:30003",
	} {
		_, err := Parse(bad)
		assert.Error(t, err, bad)
	}
}
