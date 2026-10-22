// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package sources parses the -sources / SOURCES list that selects where MDTI
// reads traffic from: public aggregators, a local receiver, or synthetic
// traffic. See docs/SOURCES.md.
package sources

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Kind is one supported source type.
type Kind string

const (
	ADSB      Kind = "adsb"      // public ADS-B aggregator chosen by ADSB_SOURCE (adsbfi | adsblol)
	AISStream Kind = "aisstream" // AISStream WebSocket (needs AISSTREAM_API_KEY); alias "ais"
	Synthetic Kind = "synthetic" // offline demo background traffic
	Readsb    Kind = "readsb"    // local receiver aircraft.json: readsb:<http URL>
	SBS       Kind = "sbs"       // local receiver BaseStation feed: sbs:<host:port>
	NMEAUDP   Kind = "nmea-udp"  // local AIS NMEA over UDP: nmea-udp:<listen addr>, e.g. nmea-udp::10110
	NMEATCP   Kind = "nmea-tcp"  // local AIS NMEA over TCP: nmea-tcp:<host:port>
)

// Spec is one parsed entry.
type Spec struct {
	Kind Kind
	Arg  string // address or URL; empty for kinds without one
}

func (s Spec) String() string {
	if s.Arg == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + ":" + s.Arg
}

// Parse reads a comma-separated list such as
// "readsb:http://192.168.1.50:8080,nmea-udp::10110,aisstream". Each entry is
// "<kind>" or "<kind>:<argument>". An empty list means no live sources.
func Parse(list string) ([]Spec, error) {
	var out []Spec
	seen := map[string]bool{}
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		kind, arg, _ := strings.Cut(raw, ":")
		s := Spec{Kind: Kind(strings.ToLower(kind)), Arg: strings.TrimSpace(arg)}
		if s.Kind == "ais" {
			s.Kind = AISStream
		}
		if err := s.validate(); err != nil {
			return nil, fmt.Errorf("source %q: %w", raw, err)
		}
		if seen[s.String()] {
			return nil, fmt.Errorf("source %q listed twice", raw)
		}
		seen[s.String()] = true
		out = append(out, s)
	}
	return out, nil
}

func (s Spec) validate() error {
	switch s.Kind {
	case ADSB, AISStream, Synthetic:
		if s.Arg != "" {
			return fmt.Errorf("%s takes no argument", s.Kind)
		}
	case Readsb:
		u, err := url.Parse(s.Arg)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("want readsb:http://<receiver>[:port][/path/aircraft.json]")
		}
	case SBS, NMEATCP:
		if _, port, err := net.SplitHostPort(s.Arg); err != nil || port == "" {
			return fmt.Errorf("want %s:<host>:<port>", s.Kind)
		}
	case NMEAUDP:
		if _, port, err := net.SplitHostPort(s.Arg); err != nil || port == "" {
			return fmt.Errorf("want nmea-udp:[host]:<port>, e.g. nmea-udp::10110")
		}
	default:
		return fmt.Errorf("unknown kind (want adsb, aisstream, synthetic, readsb, sbs, nmea-udp or nmea-tcp)")
	}
	return nil
}
