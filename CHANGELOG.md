# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] — 2026-10-21

First public release. All five SPEC phases are implemented and verified against a live Anduril Lattice Sandbox.

### Added

- **RX:** live ADS-B ingestion over AMBA (adsb.fi v3 at 1 Hz, adsb.lol fallback) and AIS over the
  Argentine EEZ (AISStream WebSocket), deterministic red-team injector (kinematic teleport/overspeed,
  baro/GNSS divergence, ghost near Morón, dark vessel at mile 200) with live-track hijacking, and
  record/replay.
- **Geography:** zone registry over unmodified Marine Regions EEZ v12 polygons and 200 NM outer
  limits, with SHA-256 pinning and a human-in-the-loop policy for disputed and joint-regime waters.
- **ING:** complete `TEMPLATE_TRACK` entities (symbology, classification, alternate ids, transponder,
  nationality by MMSI MID), a coalescing publisher with AIMD rate control, expiry policy, and
  dead-reckoning for dark vessels.
- **Tier 1:** five deterministic detectors on the stream clock, corroboration, cooldown, and
  `cmd/evaluate` scoring against ground truth.
- **Tier 2:** OpenAI Decisions API (predicate, choice and score questions), deterministic and
  versioned policy, structural identifier features (no attacker text reaches the model), guardrails,
  autonomy levels, fail-safe fallback, evidence stored in Lattice Objects, and `cmd/calibrate` with
  held-out validation and adversarial robustness checks.
- **C2:** disposition override, ING annotation, dead-reckoning trigger, ISR tasking (VisualId and
  Investigate) with one open task per target and per asset.
- **Agent:** `cmd/agent` with SENTINEL-1 and TRIDENT-2, SSE task stream, full status lifecycle,
  pursuit/orbit/trail kinematics, orphan recovery and a single-writer lock.
- **Tooling:** `cmd/validate` (separate-process read-back, task lifecycle, cancel, clear) and `cmd/demo`
  (recorded end-to-end demo) with `tools/cast2gif`.
- Open-source scaffolding: Apache-2.0, NOTICE, SECURITY, CONTRIBUTING, Code of Conduct, CI (race,
  staticcheck, govulncheck, recall and determinism gates), CodeQL, Dependabot.

[1.0.0]: https://github.com/b45t3rr/lattice-mdti/releases/tag/v1.0.0
