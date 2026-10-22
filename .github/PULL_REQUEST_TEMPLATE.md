## What and why

<!-- Link the issue. Describe the behaviour change and the SPEC sections it touches. -->

## Verification

- [ ] `go test ./...` passes; new or changed behaviour has tests (detectors: positive **and** negative)
- [ ] `make lint vuln evaluate determinism` passes
- [ ] Lattice changes were verified against a live Sandbox and read back from a separate process (`cmd/validate`). What I ran:
- [ ] Tier-2 changes: `cmd/calibrate` re-run with held-out seeds, zero false HOSTILE, `data/tier2/policy.yaml` committed
- [ ] SPEC updated in this PR

## Safety

- [ ] No RF transmission, no kinetic/effector tasking, guardrails and autonomy limits unchanged or stricter
- [ ] No secrets, Sandbox hostnames or restricted data in code, fixtures, logs or screenshots
