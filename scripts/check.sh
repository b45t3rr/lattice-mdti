#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 The MDTI Authors
#
# Every check a pull request must pass, run locally (there is no hosted CI):
# formatting, build, vet, staticcheck, tests (with the race detector when cgo
# is available), govulncheck, the Tier-1 recall gates and red-team determinism.
#
#   make check        # or: bash scripts/check.sh
#
# Exits non-zero on the first failure.
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n== %s\n' "$*"; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

step "gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "not gofmt-formatted:"; echo "$unformatted"; exit 1
fi

step "build"
go build ./...

step "vet"
go vet ./...

step "staticcheck"
go run honnef.co/go/tools/cmd/staticcheck@latest ./...

step "tests"
if [ "$(go env CGO_ENABLED)" = "1" ] && go test -race -count=1 ./internal/geo >/dev/null 2>&1; then
  go test -race -count=1 ./...
else
  echo "race detector unavailable here (needs cgo and a C compiler); running without -race"
  go test -count=1 ./...
fi

step "govulncheck"
go run golang.org/x/vuln/cmd/govulncheck@latest ./...

step "Tier-1 recall on the red-team scenarios (must be 1.0)"
S=scenarios/kinematic.yaml,scenarios/baro_gnss.yaml,scenarios/ghost_sadm.yaml,scenarios/dark_vessel_eez.yaml
for seed in 1 2 3 42; do
  go run ./cmd/redteam -scenario=$S -seed=$seed -out="$tmp/rt.jsonl" -truth="$tmp/rt.truth.jsonl" >/dev/null
  go run ./cmd/evaluate -frames="$tmp/rt.jsonl" -truth="$tmp/rt.truth.jsonl"
done

step "Tier-1 recall on the four demo acts (demo profile, must be 1.0)"
D=scenarios/demo_ghost.yaml,scenarios/demo_kinematic.yaml,scenarios/demo_gnss.yaml,scenarios/demo_dark.yaml
go run ./cmd/redteam -scenario=$D -out="$tmp/demo.jsonl" -truth="$tmp/demo.truth.jsonl" >/dev/null
TIER1_DARK_MIN_GAP=3m go run ./cmd/evaluate -frames="$tmp/demo.jsonl" -truth="$tmp/demo.truth.jsonl"

step "red-team renders are byte-identical"
go run ./cmd/redteam -scenario=$S -out="$tmp/a.jsonl" -truth="$tmp/a.truth.jsonl" >/dev/null
go run ./cmd/redteam -scenario=$S -out="$tmp/b.jsonl" -truth="$tmp/b.truth.jsonl" >/dev/null
cmp "$tmp/a.jsonl" "$tmp/b.jsonl" && cmp "$tmp/a.truth.jsonl" "$tmp/b.truth.jsonl"

printf '\nall checks passed\n'
