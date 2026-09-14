# ACB Production Handoff & Release Checklist

This template must be completed and signed by the engineering and operations leads prior to promoting any release candidate to the production environment.

---

## 1. Release Metadata

| Field | Value |
|---|---|
| **Release Version / Tag** | `vX.Y.Z` |
| **Git Commit SHA** | `abcdef123456...` |
| **Release Manifest ID** | `release-YYYYMMDD-HHMMSS` |
| **Execution Date & Time** | `YYYY-MM-DD HH:MM:SS UTC` |
| **Primary Release Engineer** | `@operator-id` |
| **Secondary Peer Reviewer** | `@reviewer-id` |

---

## 2. Pre-Promotion Verification Gates (CI/CD Pipeline)

All checks must pass before downloading artifacts or executing deployment scripts:

- [ ] **CI Pipeline Status:** All 28 test suites in `scripts/verify.sh` passed on target commit SHA.
- [ ] **Supply Chain Attestation:** Cosign signatures for first-party images verified against GitHub Actions OIDC identity.
- [ ] **Vulnerability Scanning:** Trivy container scans produced 0 critical/high unmitigated CVEs (`deploy/cve-allowlist.json` strictly enforced).
- [ ] **Third-Party Container Policy:** Bark image digest verified against `deploy/third-party-allowlist.json`.
- [ ] **Promotion Scope Classified:** `promotion-scope.json` correctly reflects modified subsystems:
  - Scope: `[ ] docs_only  [ ] gateway  [ ] worker  [ ] schema  [ ] auth_browser  [ ] tts  [ ] bark  [ ] full_stack`
- [ ] **Signed Release Manifest:** `release-manifest.json` signed with keyless Cosign bundle present.

---

## 3. Host Preflight Verification

Execute on target production VPS before rollout:

- [ ] **Disk & Volume Space:** At least 10 GB free on root filesystem and data volume (`df -h`).
- [ ] **Host Lock Available:** `/run/lock/vps-failover/acb.lock` is not locked by another deployment or failover process.
- [ ] **Secrets Integrity:** Permissions on `/opt/acb-transaction-webhook/deploy/secrets/` are `0700`, files are `0600`.
- [ ] **Clean Transaction Journal:** `deploy-journal.json` does not contain dangling uncommitted transactions (`recover_tx_journal` executed).
- [ ] **Active Authentication State:** Verified no bank authentication attempt is actively running (`-active-auth-count == 0`).
- [ ] **Fresh Database Backup:** Preflight SQLite snapshot verified and encrypted via `deploy/backup-db.sh`.

---

## 4. Release Execution Record

Record the execution command and output summary:

```bash
# Command executed:
deploy/dispatch-rollout.sh --manifest deploy/release-manifest.json --bundle deploy/release-manifest.bundle ...

# Execution Log Summary:
# - Schema Status:     [ ] SKIPPED  [ ] APPLIED
# - Sidecars Status:   [ ] SKIPPED  [ ] UPGRADED
# - Worker Status:     [ ] SKIPPED  [ ] UPGRADED (Quiesce ACK: OK)
# - Gateway Cutover:   [ ] SKIPPED  [ ] PROMOTED (From Slot: _____ To Slot: _____)
```

---

## 5. Post-Promotion Verification Matrix

Perform within 5 minutes of cutover:

- [ ] **Positive Route Identity ACK:** Edge route returns expected slot header:
  ```bash
  curl -sI -H "Host: <public_host>" http://127.0.0.1:8090/healthz | grep -E "X-Platform-Slot|X-Release-Commit"
  ```
- [ ] **Candidate Readiness:** `/internal/deployz` returns HTTP 200 with matching git revision and healthy status.
- [ ] **Worker Singleton Intact:** Exactly 1 `acb-worker` container running (`docker ps --filter name=acb-worker`).
- [ ] **Scheduler Quantum Active:** Worker logs show healthy polling quanta without starvation:
  ```bash
  docker logs --tail 40 acb-worker | grep "quantum"
  ```
- [ ] **SSE Stream Functional:** Web frontend successfully receives Server-Sent Events without reconnect loops.
- [ ] **Standby Slot Warm:** Standby gateway container is in stopped/standby state (`.intentional-stop-<slot>` present).
- [ ] **Release Evidence Archived:** Execution receipts stored in `/opt/acb-transaction-webhook/data/releases/<release_id>/`.

---

## 6. Sign-off & Production Acceptance

| Role | Signee Name | Status | Timestamp |
|---|---|:---:|:---:|
| **Release Engineer** | ________________________ | [ ] APPROVED | ________________ |
| **Platform / Ops Lead** | ________________________ | [ ] APPROVED | ________________ |
| **Backend / Worker Lead** | ________________________ | [ ] APPROVED | ________________ |
| **Security / Custodian** | ________________________ | [ ] APPROVED | ________________ |
