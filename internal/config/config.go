// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package config loads MDTI configuration from the environment (optionally
// pre-populated from a .env file) and keeps secrets out of logs (SPEC §9).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Secret is a credential that never renders its value.
type Secret string

func (s Secret) String() string               { return redacted(s) }
func (s Secret) GoString() string             { return redacted(s) }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(redacted(s)) }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted(s) + `"`), nil }

// Reveal returns the raw value; call it only at the point of use.
func (s Secret) Reveal() string { return string(s) }

func redacted(s Secret) string {
	if s == "" {
		return ""
	}
	return "[REDACTED]"
}

type Lattice struct {
	Endpoint         string // hostname only, no scheme
	EnvironmentToken Secret
	SandboxesToken   Secret
	ClientID         string
	ClientSecret     Secret
	IntegrationName  string
	MaxPublishPerSec int
}

type ADSB struct {
	Source       string // "adsbfi" | "adsblol"
	CenterLat    float64
	CenterLon    float64
	RadiusNM     float64
	PollInterval time.Duration
}

type AIS struct {
	APIKey      Secret
	BBox        [4]float64 // minLat, minLon, maxLat, maxLon
	IdleTimeout time.Duration
}

type OpenAI struct {
	APIKey         Secret
	DecisionsModel string
}

type Config struct {
	Lattice          Lattice
	OpenAI           OpenAI
	Autonomy         string // advisory | supervised | autonomous
	ADSB             ADSB
	AIS              AIS
	RedTeamSeed      int64
	Tier1Cooldown    time.Duration
	Tier2Concurrency int
	Tier2PerMinute   int
	LogLevel         slog.Level
	LogFormat        string
}

// Load reads an optional .env file (existing environment variables win) and
// parses every variable, collecting all errors instead of stopping at the first.
func Load(envFile string) (*Config, error) {
	if envFile != "" {
		if err := godotenv.Load(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load %s: %w", envFile, err)
		}
	}
	p := &parser{}
	c := &Config{
		Lattice: Lattice{
			Endpoint:         p.str("LATTICE_ENDPOINT", ""),
			EnvironmentToken: Secret(p.str("ENVIRONMENT_TOKEN", "")),
			SandboxesToken:   Secret(p.str("SANDBOXES_TOKEN", "")),
			ClientID:         p.str("LATTICE_CLIENT_ID", ""),
			ClientSecret:     Secret(p.str("LATTICE_CLIENT_SECRET", "")),
			IntegrationName:  p.str("MDTI_INTEGRATION_NAME", "mdti"),
			MaxPublishPerSec: p.int("LATTICE_MAX_PUBLISH_PER_SEC", 40),
		},
		OpenAI: OpenAI{
			APIKey:         Secret(p.str("OPENAI_API_KEY", "")),
			DecisionsModel: p.str("OPENAI_DECISIONS_MODEL", "gpt-6-luna"),
		},
		Autonomy: p.str("AUTONOMY_LEVEL", "supervised"),
		ADSB: ADSB{
			Source:       p.str("ADSB_SOURCE", "adsbfi"),
			CenterLat:    p.float("ADSB_CENTER_LAT", -34.70),
			CenterLon:    p.float("ADSB_CENTER_LON", -58.55),
			RadiusNM:     p.float("ADSB_RADIUS_NM", 60),
			PollInterval: p.dur("ADSB_POLL_INTERVAL", time.Second),
		},
		AIS: AIS{
			APIKey:      Secret(p.str("AISSTREAM_API_KEY", "")),
			BBox:        p.bbox("AIS_BBOX", [4]float64{-56, -70, -34, -52}),
			IdleTimeout: p.dur("AIS_IDLE_TIMEOUT", 120*time.Second),
		},
		RedTeamSeed:      int64(p.int("REDTEAM_SEED", 42)),
		Tier1Cooldown:    p.dur("TIER1_COOLDOWN", 120*time.Second),
		Tier2Concurrency: p.int("TIER2_CONCURRENCY", 2),
		Tier2PerMinute:   p.int("TIER2_MAX_CALLS_PER_MIN", 10),
		LogLevel:         p.level("LOG_LEVEL", slog.LevelInfo),
		LogFormat:        p.str("LOG_FORMAT", "text"),
	}
	if strings.Contains(c.Lattice.Endpoint, "://") {
		p.errs = append(p.errs, errors.New("LATTICE_ENDPOINT must be a hostname without scheme"))
	}
	switch c.Autonomy {
	case "advisory", "supervised", "autonomous":
	default:
		p.errs = append(p.errs, errors.New("AUTONOMY_LEVEL must be advisory, supervised or autonomous"))
	}
	if c.ADSB.RadiusNM <= 0 || c.ADSB.RadiusNM > 250 {
		p.errs = append(p.errs, errors.New("ADSB_RADIUS_NM must be in (0, 250]"))
	}
	if err := errors.Join(p.errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// Require fails fast if any named variable resolved to an empty value.
// Only variable names are reported, never values.
func Require(names ...string) error {
	var missing []string
	for _, n := range names {
		if strings.TrimSpace(os.Getenv(n)) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

type parser struct{ errs []error }

func (p *parser) str(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (p *parser) int(k string, def int) int {
	v := p.str(k, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: not an integer", k))
		return def
	}
	return n
}

func (p *parser) float(k string, def float64) float64 {
	v := p.str(k, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: not a number", k))
		return def
	}
	return f
}

func (p *parser) dur(k string, def time.Duration) time.Duration {
	v := p.str(k, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.errs = append(p.errs, fmt.Errorf("%s: not a positive duration", k))
		return def
	}
	return d
}

func (p *parser) bbox(k string, def [4]float64) [4]float64 {
	v := p.str(k, "")
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	if len(parts) != 4 {
		p.errs = append(p.errs, fmt.Errorf("%s: want minLat,minLon,maxLat,maxLon", k))
		return def
	}
	var b [4]float64
	for i, s := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			p.errs = append(p.errs, fmt.Errorf("%s: element %d not a number", k, i))
			return def
		}
		b[i] = f
	}
	if b[0] >= b[2] || b[1] >= b[3] {
		p.errs = append(p.errs, fmt.Errorf("%s: min must be below max", k))
		return def
	}
	return b
}

func (p *parser) level(k string, def slog.Level) slog.Level {
	v := p.str(k, "")
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: invalid log level", k))
		return def
	}
	return l
}
