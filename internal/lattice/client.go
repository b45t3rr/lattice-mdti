// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package lattice wraps the official Lattice Go SDK (REST + SSE): client
// construction for Sandboxes, symbology and literal helpers (SPEC §5.1).
package lattice

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anduril/lattice-sdk-go/v5/client"
	"github.com/anduril/lattice-sdk-go/v5/core"
	"github.com/anduril/lattice-sdk-go/v5/option"

	"github.com/b45t3rr/lattice-mdti/internal/config"
)

// Clients holds two SDK clients sharing the same credentials: Unary for
// request/response calls (30 s timeout) and Stream for SSE streams, whose
// lifetime is bounded only by the caller's context.
type Clients struct {
	Unary  *client.Client
	Stream *client.Client
	// AuthMode is "environment-token" or "client-credentials" (for logs).
	AuthMode string
}

// New builds the clients for a Lattice Sandbox. The environment token is the
// default; OAuth client credentials are used only when no token is set.
func New(cfg config.Lattice) (*Clients, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	base, err := BaseURL(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	common := []option.RequestOption{
		option.WithBaseURL(base),
		option.WithHTTPHeader(http.Header{
			"Anduril-Sandbox-Authorization": {"Bearer " + cfg.SandboxesToken.Reveal()},
		}),
		option.WithMaxAttempts(3),
	}
	mode := "environment-token"
	if tok := cfg.EnvironmentToken.Reveal(); tok != "" {
		common = append(common, option.WithToken(tok))
	} else {
		mode = "client-credentials"
		common = append(common, option.WithClientCredentials(cfg.ClientID, cfg.ClientSecret.Reveal()))
	}
	unary := client.NewClient(append(common, option.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))...)
	stream := client.NewClient(append(common, option.WithHTTPClient(&http.Client{}))...)
	return &Clients{Unary: unary, Stream: stream, AuthMode: mode}, nil
}

// Validate checks the Lattice configuration without revealing values.
func Validate(cfg config.Lattice) error {
	var errs []error
	if cfg.Endpoint == "" {
		errs = append(errs, errors.New("LATTICE_ENDPOINT is required"))
	}
	if cfg.SandboxesToken.Reveal() == "" {
		errs = append(errs, errors.New("SANDBOXES_TOKEN is required (MDTI targets Sandboxes only)"))
	}
	if cfg.EnvironmentToken.Reveal() == "" && (cfg.ClientID == "" || cfg.ClientSecret.Reveal() == "") {
		errs = append(errs, errors.New("set ENVIRONMENT_TOKEN, or LATTICE_CLIENT_ID and LATTICE_CLIENT_SECRET"))
	}
	if cfg.IntegrationName == "" {
		errs = append(errs, errors.New("MDTI_INTEGRATION_NAME must not be empty"))
	}
	return errors.Join(errs...)
}

// BaseURL turns LATTICE_ENDPOINT into the API base URL. A bare hostname
// means HTTPS. Plain HTTP is accepted only on loopback, for the offline
// sandbox (cmd/sim), so credentials never cross a network unencrypted.
func BaseURL(endpoint string) (string, error) {
	if !strings.Contains(endpoint, "://") {
		return "https://" + endpoint, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("LATTICE_ENDPOINT: %w", err)
	}
	switch {
	case u.Scheme == "https":
		return strings.TrimSuffix(endpoint, "/"), nil
	case u.Scheme == "http" && IsLoopback(u.Hostname()):
		return strings.TrimSuffix(endpoint, "/"), nil
	}
	return "", errors.New("LATTICE_ENDPOINT: plain http is only allowed on loopback (offline sandbox)")
}

// IsLoopback reports whether host names this machine.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsOffline reports whether the endpoint is the local offline sandbox.
func IsOffline(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Scheme == "http" && IsLoopback(u.Hostname())
}

// RedactEndpoint hides the environment id in a Sandbox hostname for logs and
// recordings ("<id>.env.sandboxes…" → "***.env.sandboxes…").
func RedactEndpoint(host string) string {
	if IsOffline(host) {
		return "offline sandbox " + host
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		return "***" + host[i:]
	}
	return "***"
}

// StatusCode extracts the HTTP status from an SDK error (0 if none).
func StatusCode(err error) int {
	var apiErr *core.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// Describe renders an SDK error for logs: status plus the server message,
// which names the offending field on validation errors.
func Describe(err error) string {
	if err == nil {
		return ""
	}
	if code := StatusCode(err); code != 0 {
		return fmt.Sprintf("HTTP %d: %v", code, err)
	}
	return err.Error()
}
