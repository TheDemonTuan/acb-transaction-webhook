# ACB Production Final Release Contract & Governance Specification

## 1. Executive Summary & Purpose

This document constitutes the final authoritative contract for zero-downtime, fail-closed releases and runtime operations on the single VPS host.
Every production release is guaranteed to preserve the single ACB worker running uninterrupted (unless worker promotion is explicitly scoped), execute gateway and frontend blue/green cutovers with edge verification and soak auditing, maintain canonical release state schema v2 as the single source of truth, and safely reconcile or roll back without leaving mixed or orphaned state.

---

## 2. GitHub Governance & Branch Protection Ruleset

To prevent unverified mutations from reaching the production environment, the `main` branch is governed by strict ruleset enforcement:

### 2.1 Ruleset Configuration for `main`
- **Require pull request before merging**: ON
- **Required approvals**: 0 or 1 (solo maintainer choice)
- **Dismiss stale pull request approvals on new commits**: ON
- **Require status checks to pass before merging**: ON
  - Required check: `Production Contract`
- **Require branches to be up to date before merging**: ON
- **Require linear history**: ON
- **Block force pushes**: ON
- **Block branch deletions**: ON
- **Bypass**: Disabled for normal pushes (emergency bypass strictly audited via GitHub Organization admin log).

### 2.2 Production Secret Isolation
- CI jobs running on Pull Requests or untrusted branches execute in an isolated environment with **zero access** to VPS SSH keys, production secrets, or signing credentials.
- Deploy jobs require the protected `production` environment and are locked to push events on `refs/heads/main`.
- Production concurrency is strictly serialized:
  ```yaml
  concurrency:
    group: acb-transaction-webhook-production
    cancel-in-progress: false
  ```

---

## 3. Architecture: Immutable Release Bundle vs Shared Runtime State

### 3.1 Immutable Release Bundle (`releases/<release_id>`)
Each release is staged in an immutable directory signed with Cosign and hashed in `release-manifest.json`:
- Compose fragments: `compose/{base,gateway,frontend,worker,auth-browser,tts,bark,dbtool}.yaml`
- Deployment engine scripts: `dispatch-rollout.sh`, `runtime-layout.sh`, `reconcile-release.sh`, `release-state.py`, etc.
- No mutable files, runtime logs, socket files, or locks may ever reside inside release directories.

### 3.2 Shared Mutable Runtime State (`deploy/runtime-layout.sh`)
All mutable runtime state resolves strictly under `$RUNTIME_ROOT` (`/opt/acb-transaction-webhook`):
```text
$RUNTIME_ROOT/
├── deploy/                     # Installed deployment engine & configuration
│   ├── .env.production
│   ├── .release.env            # Projection only, generated after canonical commit
│   └── secrets/                # Encrypted/chmod-protected secrets
├── state/                      # Authoritative state and slot pointers
│   ├── current-release.json    # Canonical release state schema v2
│   ├── gateway-active-slot     # blue | green
│   ├── gateway-previous-slot
│   ├── frontend-active-slot    # blue | green
│   ├── deploy-state.json
│   └── candidate/              # Pre-commit candidate release state
└── data/                       # Operational data, journals, and backups
    ├── deploy-journal.json     # Component transaction journal
    ├── rollout-journal.json    # Release rollout journal
    ├── pending-gateway-retire.env
    ├── pending-frontend-retire.env
    └── backups/
```

---

## 4. Release State Machine & Invariants

```text
[Pre-Flight Verification]
  │
  ├─ 1. Check runtime dirty / previous INTERRUPTED status
  │    └─ If dirty: execute recovery-only reconciliation before image builds
  │
  ├─ 2. Build candidate-release.json schema v2 directly from manifest + previous canonical state
  │
  ├─ 3. Promote scoped components in dependency order:
  │    Schema -> Auxiliaries (Auth-Browser, TTS, Bark) -> Frontend -> Worker -> Gateway -> Failover
  │
  ├─ 4. Pre-soak contract audit: verify active routes and digests match planned candidate state
  │    └─ Fails within seconds if any mismatch, preventing wasteful 900s soak
  │
  ├─ 5. 900s Gateway Soak observation
  │
  ├─ 6. Final runtime drift check
  │
  ├─ 7. Atomic canonical commit (current-release.json updated to schema v2)
  │
  ├─ 8. Regenerate .release.env compatibility projection from committed canonical state
  │
  └─ 9. Retire old standby slots & archive rollout journal
```

---

## 5. Rollback & Recovery Invariants

1. **Non-Short-Circuiting Rollback**: Every restoration step (gateway route, frontend route, completed components) is attempted exhaustively even if an earlier step encounters an issue.
2. **Exact Previous Release Restoration**: Rollback uses `RELEASE_CONTEXT_DIR` pointing to the exact previous release bundle directory, restoring previous container images **and** previous Compose configs.
3. **Legacy Frontend Recovery**: If previous frontend topology was `legacy` (`acb-frontend`), rollback restores route to `acb-frontend`, deletes `frontend-active-slot`, verifies route ACK, and terminates the candidate B/G container only after ACK.
4. **Bootstrap Recovery Path**: `deploy/bootstrap-deployment-engine.sh` allows upgrading trusted deployer and reconciliation tools from a signed release without triggering application container mutations.

---

## 6. Emergency Runbook

### 6.1 Manual Interrupted Rollout Reconciliation
If a deployment was interrupted (network loss, runner abort):
```bash
ssh <vps-user>@<vps-host>
sudo bash /opt/acb-transaction-webhook/deploy/reconcile-release.sh \
  --runtime-root /opt/acb-transaction-webhook \
  --recovery-only
```

### 6.2 Manual Rollback to Previous Canonical Release
```bash
sudo bash /opt/acb-transaction-webhook/deploy/rollback-release.sh \
  --state /opt/acb-transaction-webhook/state/current-release.json \
  --journal /opt/acb-transaction-webhook/data/rollout-journal.json
```

### 6.3 Runtime Drift Audit
```bash
sudo bash /opt/acb-transaction-webhook/deploy/verify-runtime-drift.sh \
  --state /opt/acb-transaction-webhook/state/current-release.json
```
Expected output: `Runtime matches canonical release state.`
