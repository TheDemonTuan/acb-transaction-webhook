# Historical & Superseded Architecture Archive

This directory archives historical planning documents, early migration proposals, architectural drafts, and fix notes generated during the evolution of the ACB Transaction Webhook platform.

> **CRITICAL OPERATOR NOTICE:**
> All documents in this directory are **SUPERSEDED** and retained strictly for historical context, design lineage, and forensic audit.
> **DO NOT** execute procedures, implement requirements, or configure production environments based on any file in this archive.
>
> **Authoritative Production Specifications:**
> 1. Invariant Specification: [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](../superpowers/specs/2026-09-14-acb-final-production-invariants.md)
> 2. Production Architecture: [`docs/architecture/PRODUCTION_ARCHITECTURE.md`](../architecture/PRODUCTION_ARCHITECTURE.md)
> 3. Compatibility Contracts: [`docs/architecture/COMPATIBILITY_CONTRACTS.md`](../architecture/COMPATIBILITY_CONTRACTS.md)
> 4. System Inventory: [`docs/architecture/SYSTEM_INVENTORY.md`](../architecture/SYSTEM_INVENTORY.md)
> 5. Convergence Execution Plan: [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](../superpowers/plans/2026-09-14-acb-production-convergence-execution.md)

---

## Catalog of Superseded Documents

| Document | Date | Original Purpose | Reason Superseded |
|---|:---:|---|---|
| `2026-09-12-caddy-progressive-blue-green-deployment.md` | 2026-09-12 | Early proposal for per-app Caddy dynamic route switching and progressive rollout. | Superseded by shared Traefik 3.x edge ingress under `/opt/edge` to avoid duplicate ingress layers and multi-tenant port conflicts. |
| `2026-09-13-acb-secure-single-vps-platform-migration.md` | 2026-09-13 | Initial single-VPS hardening and network segmentation plan. | Refined into canonical 2026-09-14 architecture with decoupled worker/gateway lifecycles and strict fail-closed auth admission. |
| `2026-09-13-single-vps-secure-container-platform-standard.md` | 2026-09-13 | Host security standard proposal for single-VPS container hosting. | Formalized in `docs/architecture/PRODUCTION_ARCHITECTURE.md` and `docs/runbooks/SECRET_PROVISIONING_RUNBOOK.md`. |
| `2026-09-14-acb-final-production-correctness-hardening.md` | 2026-09-14 | Working draft implementation plan for 14-phase production correctness hardening. | Formalized as canonical execution plan in `docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`. |
| `2026-09-14-acb-final-production-invariants.md` | 2026-09-14 | Working draft invariant specification. | Established as tracked canonical specification at `docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`. |
| `fix.md` | 2026-09-13 | Early tactical fix notes on session state and SQLite locking. | Addressed permanently by priority scheduler quantum yield and async worker RPC architecture. |
| `fix_2.md` | 2026-09-13 | Tactical notes on Traefik configuration and header forwarding. | Incorporated into `deploy/switch-slot.sh` with positive `X-Platform-Slot` ACK. |
| `fix_3.md` | 2026-09-13 | Notes on worker singleton lifecycle and graceful shutdown. | Formalized in `deploy/deploy-worker.sh` and `/rpc/quiesce` handoff protocol. |
| `fix_4.md` | 2026-09-13 | Hardening notes on Cosign verification and digest pinning. | Fully implemented in CI promotion workflow (`.github/workflows/deploy.yml`) and `deploy/verify-manifest.sh`. |
| `deploy_fix.md` | 2026-09-13 | Intermediate notes on deploy script syntax and environment variables. | Resolved by unified `compose.prod.yaml` and modular `deploy/lib/*.sh` libraries. |
| `FIX_BARK_PROVIDER_REVIEW.md` | 2026-09-13 | Review notes on Bark push notification provider integration. | Bark container isolated as independent auxiliary sidecar (`deploy/deploy-bark.sh`) on internal `acb-core` network. |
| `FIX_BARK_PROVIDER_REVIEW (1).md` | 2026-09-13 | Duplicate working copy of Bark provider review. | Superseded along with primary Bark review. |
| `Public Transaction Viewer - Cloudflare Access Split — Implementation Plan.md` | 2026-09-14 | Planning document for public transaction viewer and Cloudflare Access separation. | Completed in PR07 / `cmd/gateway` route segmentation. |
| `last_plan.md`, `new_fix.md`, `new_fix_2.md`, `newplan.md` | 2026-09-13–14 | Ephemeral session notes and scratchpads from multi-stage hardening. | Replaced by master tracker in `docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`. |
