// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package redteam is the deterministic adversary injector (SPEC §4.4).
// It operates on data only: nothing here transmits RF.
package redteam

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Attack identifies a generator.
type Attack string

const (
	AttackKinematic  Attack = "kinematic"
	AttackBaroGNSS   Attack = "baro_gnss"
	AttackGhost      Attack = "ghost"
	AttackDarkVessel Attack = "dark_vessel"
)

// TargetMode selects whether the attack hijacks a live track or synthesizes one.
type TargetMode string

const (
	TargetSynthesize TargetMode = "synthesize"
	TargetHijack     TargetMode = "hijack"
)

// Scenario is one YAML file under scenarios/.
type Scenario struct {
	Name       string        `yaml:"name"`
	Attack     Attack        `yaml:"attack"`
	Seed       int64         `yaml:"seed"`
	StartAfter time.Duration `yaml:"start_after"`
	Target     struct {
		Mode       TargetMode    `yaml:"mode"`
		HijackWait time.Duration `yaml:"hijack_wait"` // fall back to synthesize after this
	} `yaml:"target"`
	Params yaml.Node `yaml:"params"`
}

// LoadScenario reads and validates a scenario file.
func LoadScenario(path string) (*Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Target.Mode == "" {
		s.Target.Mode = TargetSynthesize
	}
	if s.Target.HijackWait == 0 {
		s.Target.HijackWait = 60 * time.Second
	}
	switch s.Attack {
	case AttackKinematic, AttackBaroGNSS, AttackGhost, AttackDarkVessel:
	default:
		return nil, fmt.Errorf("%s: unknown attack %q", path, s.Attack)
	}
	if s.Target.Mode == TargetHijack && (s.Attack == AttackGhost || s.Attack == AttackDarkVessel) {
		return nil, fmt.Errorf("%s: attack %q always synthesizes its target", path, s.Attack)
	}
	if _, err := s.generator(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// generator decodes the typed params (with defaults) for the attack.
func (s *Scenario) generator() (generator, error) {
	var g generator
	switch s.Attack {
	case AttackKinematic:
		p := defaultKinematic()
		g = &p
	case AttackBaroGNSS:
		p := defaultBaroGNSS()
		g = &p
	case AttackGhost:
		p := defaultGhost()
		g = &p
	case AttackDarkVessel:
		p := defaultDarkVessel()
		g = &p
	}
	if !s.Params.IsZero() {
		if err := s.Params.Decode(g); err != nil {
			return nil, fmt.Errorf("params: %w", err)
		}
	}
	return g, g.validate()
}
