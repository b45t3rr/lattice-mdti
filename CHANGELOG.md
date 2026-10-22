# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Your own receivers as sources** (`-sources` / `SOURCES`, `docs/SOURCES.md`):
  - `readsb:<url>` for a receiver's `aircraft.json`;
  - `sbs:<host:port>` for the BaseStation feed on port 30003;
  - `nmea-udp:` / `nmea-tcp:` for AIS NMEA `!AIVDM`, with multi-fragment reassembly and static
    data joined; decoder `github.com/BertoldVdb/go-ais`, MIT.

  `tools/receiver-sim` is a fake receiver for trying the adapters without hardware.
- **Evidence on the map:** every alert states the measured inconsistency (implied speed and Mach,
  GNSS-vs-baro offset, first-fix distance, AIS silence and distance to the 200 NM line), and draws
  its geometry (the jump line, the first-fix marker).
- **Detection-only C2** (`mdti -tasking=false`): classify and draw the evidence, but never
  dispatch an asset.
- **Four-act demo, detection only:** ghost, impossible physics (`scenarios/demo_kinematic.yaml`),
  GNSS spoofing, AIS switched off at the Milla 201.
- **Offline demo mode** (`make demo-offline`): runs the whole pipeline with no Lattice or OpenAI
  account. `cmd/sim` is a loopback-only, in-memory stand-in for the Lattice REST subset MDTI uses
  (entities with overrides and expiry, tasks with the agent SSE stream and strictly increasing
  status versions, objects). It also has a Decisions API stub that replays the answers recorded in
  the live run (reported as `offline-stub`), and a live map at `http://127.0.0.1:8740/`. The
  binaries are unchanged: only `LATTICE_ENDPOINT` and `OPENAI_BASE_URL` point at it.
- **Synthetic background traffic** (`mdti -sources=synthetic`): `SYN`-labelled airliners and
  vessels. Tier 1 is tested to raise nothing on 30 minutes of it.
- **Map layers in the C2** (`internal/viz`): watch rings, ADS-B coverage, 200 NM lines, alert
  markers, RF search areas, dead-reckoned tracks, sensor rings, intercept lines, patrol stations.
- C2 picks the nearest idle asset of the right domain. A single-writer lock stops two agents from
  driving the same asset.

### Fixed

- A receiver's own `aircraft.json` reports `now` in seconds; it was read as milliseconds.
- `cmd/demo -hold` was cut short by the overall timeout, which also killed the child processes.
- Point objectives (RF emitter search) were reported as "target lost".
- The Argentine 200 NM line south of 38°S was drawn as a disputed limit: outer-limit polylines
  are now labelled by Marine Regions `line_id`.
- `LATTICE_ENDPOINT` accepts `http://` only on loopback (the offline sandbox).

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
