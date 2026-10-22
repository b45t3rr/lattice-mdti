// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretNeverRenders(t *testing.T) {
	s := Secret("super-secret-token")
	assert.Equal(t, "[REDACTED]", s.String())
	assert.Equal(t, "[REDACTED]", fmt.Sprintf("%v %s %#v", s, s, s)[:10])
	assert.NotContains(t, fmt.Sprintf("%v %s %#v %+v", s, s, s, struct{ S Secret }{s}), "super-secret")
	b, err := json.Marshal(struct{ T Secret }{s})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "super-secret")

	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "token", s)
	assert.NotContains(t, buf.String(), "super-secret")
	assert.Equal(t, "super-secret-token", s.Reveal())
	assert.Equal(t, "", Secret("").String(), "empty stays empty")
}

func TestLoadDefaultsAndOverrides(t *testing.T) {
	t.Setenv("ADSB_RADIUS_NM", "80")
	t.Setenv("ADSB_POLL_INTERVAL", "2s")
	t.Setenv("AIS_BBOX", "-50,-68,-38,-55")
	t.Setenv("ENVIRONMENT_TOKEN", "tok")
	c, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "adsbfi", c.ADSB.Source)
	assert.Equal(t, 80.0, c.ADSB.RadiusNM)
	assert.Equal(t, 2*time.Second, c.ADSB.PollInterval)
	assert.Equal(t, [4]float64{-50, -68, -38, -55}, c.AIS.BBox)
	assert.Equal(t, "tok", c.Lattice.EnvironmentToken.Reveal())
	assert.Equal(t, "mdti", c.Lattice.IntegrationName)
}

func TestLoadDotEnvDoesNotOverrideEnvironment(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(p, []byte("MDTI_INTEGRATION_NAME=from-file\nREDTEAM_SEED=7\n"), 0o600))
	t.Setenv("MDTI_INTEGRATION_NAME", "from-env")
	t.Setenv("REDTEAM_SEED", "")
	c, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, "from-env", c.Lattice.IntegrationName)
	_, err = Load(filepath.Join(dir, "missing.env"))
	assert.NoError(t, err, "a missing .env is fine")
}

func TestLoadCollectsAllErrors(t *testing.T) {
	t.Setenv("LATTICE_ENDPOINT", "https://x.example.com")
	t.Setenv("ADSB_RADIUS_NM", "400")
	t.Setenv("ADSB_POLL_INTERVAL", "fast")
	t.Setenv("AIS_BBOX", "1,2,3")
	t.Setenv("LOG_LEVEL", "loud")
	_, err := Load("")
	require.Error(t, err)
	for _, want := range []string{"LATTICE_ENDPOINT", "ADSB_RADIUS_NM", "ADSB_POLL_INTERVAL", "AIS_BBOX", "LOG_LEVEL"} {
		assert.ErrorContains(t, err, want)
	}
}

func TestRequireListsNamesOnly(t *testing.T) {
	t.Setenv("MDTI_TEST_SET", "value-should-not-leak")
	t.Setenv("MDTI_TEST_EMPTY", " ")
	err := Require("MDTI_TEST_SET", "MDTI_TEST_EMPTY", "MDTI_TEST_UNSET_XYZ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MDTI_TEST_EMPTY, MDTI_TEST_UNSET_XYZ")
	assert.NotContains(t, err.Error(), "value-should-not-leak")
}
