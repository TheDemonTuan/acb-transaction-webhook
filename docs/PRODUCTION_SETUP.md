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

# Production bootstrap — one time

GitHub Actions can build and deploy the gateway after these one-time steps. It cannot safely create a VPS, Cloudflare account, Access application, ACB session, backup account or secret store on the operator's behalf.

## 1. VPS prerequisites

- Linux VPS with local SSD, Docker Engine + Compose v2, a non-root deploy user, and no public Docker API.
- Install GitHub Container Registry access for the deploy user if the package is private.
- Create `${DEPLOY_PATH:-/opt/acb-transaction-webhook}/deploy`, `${DEPLOY_PATH}/secrets`, owned by the deploy user.
- Copy `.env.example` into `${DEPLOY_PATH}/deploy/.env.production`, replace all placeholders, and restrict it to `0600`.
- Generate one key: `openssl rand -base64 32 > ${DEPLOY_PATH}/deploy/secrets/app_master_key`; `chmod 600` it. Confirm it decodes to exactly 32 bytes.
- Set backup destination and perform a restore drill before activation. The current backup script deliberately refuses a live SQLite copy; online backup is a later implementation gate.

## 2. Cloudflare Access and Tunnel

- Create a dedicated Tunnel whose ingress only routes the gateway hostname to `http://gateway:8080`.
- Create an Access application and policy for operator identities.
- Put the issuer, audience and JWKS URL into `.env.production`.
- Obtain the immutable JWT **subject** values issued by Access and put them in `OWNER_SUBJECTS` / `OPERATOR_SUBJECTS` / `VIEWER_SUBJECTS`.
- Do not expose VNC, CDP, `/metrics`, future browser RPC or the Docker socket.

## 3. GitHub repository configuration

Create a protected `production` environment and configure these secrets:

| Secret | Purpose |
|---|---|
| `VPS_HOST`, `VPS_USER`, optional `VPS_PORT` | deploy target |
| `VPS_SSH_KEY` | deploy user's restricted Ed25519 private key |
| `VPS_KNOWN_HOSTS` | pinned `ssh-keyscan` output verified by operator |

Create repository variable `DEPLOY_PATH` if not using `/opt/acb-transaction-webhook`. Require environment approval for production deployments. Actions uploads deployment manifests and deploys the image digest emitted by Buildx; it never deploys a mutable `latest` tag.

## 4. Current safety boundary

The deployment pipeline can deploy the foundation today. It cannot yet make the system monitor ACB because browser sandboxing, manual login, session handoff, dynamic ACB request fields, pagination and rate limits are intentionally unimplemented pending an authorized live proof of concept.
