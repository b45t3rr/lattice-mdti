// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package lattice

import (
	"errors"
	"testing"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/config"
)

func TestSIDC(t *testing.T) {
	tests := []struct {
		d    Lattice.MilViewDisposition
		e    Lattice.MilViewEnvironment
		f    Function
		want string
	}{
		{Lattice.MilViewDispositionDispositionHostile, Lattice.MilViewEnvironmentEnvironmentAir, FuncCivilFixedWing, "SHAPCF---------"},
		{Lattice.MilViewDispositionDispositionNeutral, Lattice.MilViewEnvironmentEnvironmentSurface, FuncFishingVessel, "SNSPXF---------"},
		{Lattice.MilViewDispositionDispositionFriendly, Lattice.MilViewEnvironmentEnvironmentAir, FuncMilitaryUAV, "SFAPMFQ--------"},
		{Lattice.MilViewDispositionDispositionSuspicious, Lattice.MilViewEnvironmentEnvironmentSurface, FuncUnspecified, "SSSP-----------"},
		{Lattice.MilViewDispositionDispositionUnknown, Lattice.MilViewEnvironmentEnvironmentAir, "bad", "SUAP-----------"},
		{Lattice.MilViewDispositionDispositionPending, Lattice.MilViewEnvironmentEnvironmentLand, FuncUnspecified, "SUGP-----------"},
	}
	for _, tc := range tests {
		got := SIDC(tc.d, tc.e, tc.f)
		assert.Equal(t, tc.want, got)
		assert.Len(t, got, 15)
	}
}

func TestNationalityLiteralsExistInSDK(t *testing.T) {
	// The SDK's string enums accept anything locally; the server rejects
	// unknown literals. Parse every mapped value with the SDK's own parser.
	for mid, n := range midNationality {
		_, err := Lattice.NewMilViewNationalityFromString(string(n))
		assert.NoError(t, err, "MID %s → %s", mid, n)
	}
}

func TestNationalityFromMMSI(t *testing.T) {
	n, ok := NationalityFromMMSI("701000677")
	require.True(t, ok)
	assert.Equal(t, Lattice.MilViewNationality("NATIONALITY_ARGENTINA"), n)
	_, ok = NationalityFromMMSI("538009889") // Marshall Islands: no Lattice literal
	assert.False(t, ok)
	_, ok = NationalityFromMMSI("200000001") // unallocated MID
	assert.False(t, ok)
	_, ok = NationalityFromMMSI("970123456") // AIS-SART, not a ship station
	assert.False(t, ok)
	_, ok = NationalityFromMMSI("70100")
	assert.False(t, ok)
}

func TestValidate(t *testing.T) {
	ok := config.Lattice{Endpoint: "x.env.sandboxes.developer.anduril.com", EnvironmentToken: "t", SandboxesToken: "s", IntegrationName: "mdti"}
	assert.NoError(t, Validate(ok))

	oauth := ok
	oauth.EnvironmentToken, oauth.ClientID, oauth.ClientSecret = "", "id", "secret"
	assert.NoError(t, Validate(oauth))
	c, err := New(oauth)
	require.NoError(t, err)
	assert.Equal(t, "client-credentials", c.AuthMode)

	bad := config.Lattice{}
	err = Validate(bad)
	for _, want := range []string{"LATTICE_ENDPOINT", "SANDBOXES_TOKEN", "ENVIRONMENT_TOKEN", "MDTI_INTEGRATION_NAME"} {
		assert.ErrorContains(t, err, want)
	}
}

func TestStatusCode(t *testing.T) {
	err := core.NewAPIError(404, nil, errors.New("not found"))
	assert.Equal(t, 404, StatusCode(err))
	assert.Equal(t, 0, StatusCode(errors.New("x")))
	assert.Contains(t, Describe(err), "HTTP 404")
}

func TestRedactEndpoint(t *testing.T) {
	assert.Equal(t, "***.env.sandboxes.developer.anduril.com", RedactEndpoint("abc123.env.sandboxes.developer.anduril.com"))
	assert.Equal(t, "***", RedactEndpoint("localhost"))
}

func TestBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"abc.env.sandboxes.developer.anduril.com": "https://abc.env.sandboxes.developer.anduril.com",
		"https://abc.example.com/":                "https://abc.example.com",
		"http://127.0.0.1:8740":                   "http://127.0.0.1:8740",
		"http://localhost:8740":                   "http://localhost:8740",
		"http://[::1]:8740":                       "http://[::1]:8740",
	} {
		got, err := BaseURL(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"http://abc.example.com", "http://10.0.0.5:8740", "ftp://127.0.0.1"} {
		_, err := BaseURL(bad)
		assert.Error(t, err, bad)
	}
	assert.True(t, IsOffline("http://127.0.0.1:8740"))
	assert.False(t, IsOffline("abc.env.sandboxes.developer.anduril.com"))
	assert.Equal(t, "offline sandbox http://127.0.0.1:8740", RedactEndpoint("http://127.0.0.1:8740"))
}
