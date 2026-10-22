SCENARIOS := scenarios/kinematic.yaml,scenarios/baro_gnss.yaml,scenarios/ghost_sadm.yaml,scenarios/dark_vessel_eez.yaml

.PHONY: check build test vet lint vuln rx redteam replay determinism evaluate calibrate agent demo demo-offline sim receiver-sim

build:
	go build -o bin/ ./cmd/... ./tools/...

# Every check a pull request must pass (there is no hosted CI): gofmt, build,
# vet, staticcheck, tests (-race when cgo is available), govulncheck, recall
# gates and red-team determinism.
check:
	bash scripts/check.sh

test:
	go test ./...

vet:
	go vet ./...

lint: vet
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Live RX dry run (ADS-B always; AIS when AISSTREAM_API_KEY is set).
rx:
	go run ./cmd/mdti -modules=rx -dry-run

# Render all scenarios offline into a replayable file + ground truth.
redteam:
	go run ./cmd/redteam -scenario=$(SCENARIOS) -out=redteam.jsonl -truth=groundtruth.jsonl

replay: redteam
	go run ./cmd/mdti -modules=rx -dry-run -replay=redteam.jsonl -replay-speed=0

# Phase 1 acceptance: two renders with the same seed are byte-identical.
determinism:
	go run ./cmd/redteam -scenario=$(SCENARIOS) -out=bin/a.jsonl -truth=bin/a.truth.jsonl
	go run ./cmd/redteam -scenario=$(SCENARIOS) -out=bin/b.jsonl -truth=bin/b.truth.jsonl
	cmp bin/a.jsonl bin/b.jsonl && cmp bin/a.truth.jsonl bin/b.truth.jsonl && echo "deterministic: OK"

# Phase 3 gate: Tier-1 recall on all scenarios (exits non-zero below 1.0).
evaluate: redteam
	go run ./cmd/evaluate -frames=redteam.jsonl -truth=groundtruth.jsonl -v

# Phase 4: Tier-2 threshold calibration with held-out validation (needs OPENAI_API_KEY).
calibrate:
	go run ./cmd/calibrate -seeds 1,2,3 -test-seeds 4,5,6

# Phase 5: simulated ISR assets (separate process).
agent:
	go run ./cmd/agent -dwell-vid 60s

# Recorded end-to-end demo: runs the agent + daemon as separate processes,
# narrates the pipeline, follows the task, cleans up, and renders the README GIF.
demo: build
	./bin/demo -cast docs/demo/demo.cast
	go run ./tools/cast2gif -in docs/demo/demo.cast -out docs/demo/demo.gif -idle 1.2

# Offline demo: no Lattice or OpenAI account. Starts the offline sandbox
# (simulated Lattice + Decisions stub with recorded answers), opens its live
# map in the browser and runs the same three acts on synthetic traffic.
demo-offline: build
	./bin/demo -offline -hold 2m

# Offline sandbox on its own (map at http://127.0.0.1:8740/).
sim:
	go run ./cmd/sim

# A fake local receiver (SBS :30003, aircraft.json :8080, AIS NMEA to UDP :10110) for trying
# the local-source adapters without hardware; one aircraft jumps 12 NM after 60 s.
receiver-sim:
	go run ./tools/receiver-sim -teleport 60s
