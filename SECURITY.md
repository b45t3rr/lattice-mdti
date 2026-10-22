# Security Policy

## Reporting a vulnerability

Please **do not open public issues** for security problems. Report them privately via
[GitHub Security Advisories](https://github.com/b45t3rr/lattice-mdti/security/advisories/new).
You can expect an acknowledgement within 7 days. We follow coordinated disclosure.

In scope: credential leakage, prompt-injection bypasses of the Tier-2 guardrails, guardrail or
autonomy-level bypasses, unsafe parsing of telemetry input, and dependency vulnerabilities.

## Scope and dual-use statement

MDTI is a **defensive** research project that detects spoofing and evasion in open surveillance
protocols (ADS-B, AIS).

- **No RF transmission.** The red-team injector operates only on data inside the process. The
  repository contains no code that transmits over the air, and contributions adding RF transmission
  capability will not be accepted.
- **ISR tasking only.** Autonomous responses are limited to identification and investigation tasks,
  enforced by a hard-coded allow-list. Kinetic or effector tasking is out of scope.
- **Sandbox only.** The project targets Anduril Lattice Sandboxes and is not intended for operational
  deployment.
- **Human-on-the-loop** by default (`AUTONOMY_LEVEL=supervised`). In disputed zones, escalation always
  requires an operator.

Transmitting on aviation or maritime frequencies without authorization is illegal in most
jurisdictions. Do not use knowledge from this project to interfere with real-world navigation or
surveillance systems.

## Secrets

Never commit `.env` or any token. Credentials are loaded from the environment and redacted in logs.
If you accidentally expose a Lattice, AISStream or OpenAI credential, rotate it immediately.
