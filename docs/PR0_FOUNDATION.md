> **SUPERSEDED / HISTORICAL ARCHIVE**
>
> This document is retained solely for historical context, audit trails, and design lineage.
> It has been superseded by the canonical 2026-09-14 production architecture and hardening specifications:
> - **Canonical Specification:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](superpowers/specs/2026-09-14-acb-final-production-invariants.md)
> - **Production Architecture:** [`docs/architecture/PRODUCTION_ARCHITECTURE.md`](architecture/PRODUCTION_ARCHITECTURE.md)
> - **Execution Plan & Tracker:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](superpowers/plans/2026-09-14-acb-production-convergence-execution.md)
>
> Do not implement, deploy, or operate against this document.

---

# PR 0 foundation status

## Completed locally

- Bun was upgraded from 1.4.0 to **1.4.2**.
- Go **1.27.1** was downloaded from `go.dev`, checked against the SHA-256 in the official download JSON, and installed under `~/.local/go-1.27.1` for this workstation.
- React/Vite bundle is built with `bunx --bun` and embedded in the Go gateway.
- SQLite opens with WAL, `synchronous=FULL`, foreign keys, bounded busy timeout and a local process `flock`.
- The foundation status API, health/readiness routes, static SPA fallback, AES-GCM envelope primitives, migration checksum handling, deployment skeleton and desktop/mobile browser tests are working locally.

## Not production-ready yet

No ACB request, parser, cookies, browser controller, Cloudflare JWT verification, role enforcement, actual webhook dispatch, SSRF guard, alert queue, real backup, Docker build, or VPS sandbox test has been implemented. No live bank login was attempted. These remain explicit gates, not placeholders that can be enabled safely.

## Required next proof

1. On the target VPS, run an authorized manual login in a sandboxed non-root Chrome container.
2. Prove, without persisting credentials, that the authenticated cookie/state can be transferred to the Go HTTP client after browser shutdown.
3. Record scrubbed fixtures that prove current hidden fields, pagination termination, transaction identity, session expiry and rate behavior.
4. Only then implement the ACB adapter, monitor, browser-controller, auth and webhook phases.
