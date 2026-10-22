# Contributing to MDTI

Thanks for your interest! MDTI is a defensive R&D project. Please read [`SECURITY.md`](SECURITY.md)
(scope and dual-use statement) and the [Code of Conduct](CODE_OF_CONDUCT.md) before contributing.

## Ground rules

- **Spec first.** [`SPEC.md`](SPEC.md) is the source of truth. Behaviour changes start as a SPEC change
  in the same pull request (what, why, how it is verified).
- **Out of scope**, and pull requests for these will be closed: RF transmission of any kind, kinetic or
  effector tasking, removing or weakening guardrails, autonomy limits or the single-writer lock, and
  production-tenant (non-Sandbox) deployment.
- **No secrets** in code, tests, fixtures, logs, screenshots or issues. Tokens come from the
  environment (`.env`, never committed).
- **No restricted data.** Do not add recordings from sources whose terms forbid redistribution. Live
  ADS-B and AIS recordings stay local (`*.jsonl` is ignored). See [`NOTICE`](NOTICE).

## Development setup

Requirements: Go ≥ 1.27.3 (the `toolchain` directive downloads it automatically) and `make` (optional).

```sh
git clone https://github.com/b45t3rr/lattice-mdti && cd lattice-mdti
go test ./...                 # unit, contract, replay and scenario tests (no network)
make lint vuln                # go vet + staticcheck, govulncheck
make evaluate determinism     # Tier-1 recall gate, red-team determinism
```

Most work needs no account at all: `make demo-offline` runs the whole pipeline on the offline
sandbox, and `go run ./tools/receiver-sim` stands in for a local receiver.

**New source adapters are very welcome.** Follow the guide in [`docs/SOURCES.md`](docs/SOURCES.md)
("Write a new source"), and test with synthetic data only.

Live checks need a Lattice Sandbox (`LATTICE_ENDPOINT`, `ENVIRONMENT_TOKEN`, `SANDBOXES_TOKEN`), an
`AISSTREAM_API_KEY` and an `OPENAI_API_KEY` in `.env` (see [`.env.example`](.env.example)).

## Pull requests

1. Open an issue first for anything non-trivial, so the approach can be agreed against the SPEC.
2. Keep changes focused and follow the existing code style (`gofmt`, small packages, comments that
   explain *why*).
3. Add or update tests. Detectors need positive **and** negative cases, and policy changes need
   table-driven tests.
4. **Lattice changes** must be verified against a live Sandbox and read back from a separate process
   (`cmd/validate`); say in the PR what you ran. Enum literals and field names must come from the SDK,
   because wrong values pass locally and are rejected only by the server.
5. **Tier-2 changes** (brief, questions, evidence, policy) must re-run `cmd/calibrate` with held-out
   seeds and keep **zero false HOSTILE** on benign controls. Commit the regenerated
   `data/tier2/policy.yaml`.
6. CI must be green: build, vet, staticcheck, tests with `-race`, govulncheck, Tier-1 recall gate and
   determinism.
7. Every new Go file starts with the SPDX header:

   ```go
   // SPDX-License-Identifier: Apache-2.0
   // Copyright 2026 The MDTI Authors
   ```

By contributing you agree that your contribution is licensed under the [Apache License 2.0](LICENSE).

## Reporting security issues

Do **not** open public issues for vulnerabilities. Follow [`SECURITY.md`](SECURITY.md).
