# ACB Session Recovery: Operations and Acceptance

> Updated 2026-10-02. This runbook replaces the earlier implementation checklist, human-only CAPTCHA restriction, optional AI page classifier and per-generation retry policy. The authoritative architecture is [ACB Session Recovery Design](2026-09-15-acb-session-recovery-design.md). The implemented path is **optional 9router crop-only CAPTCHA OCR first → private Telegram human fallback → always human login OTP → worker verification → day-by-day catch-up → automatic gate release**. Existing manual admin/noVNC auth remains available.

## One-command VPS setup

After this release's pipeline has deployed successfully, run once on the VPS:

```bash
sudo bash /opt/bank-event-gateway/deploy/setup-recovery.sh
```

For a nondefault deployment root, append `--deploy-path /actual/root`. The workflow publishes the stable entrypoint only after committed release and route-health checks. The wrapper resolves its release companion through the symlink; both files and the AI override are included in release checksums and SCP transfer.

The script verifies the installed bundle, image and schema, reuses existing master key/internal tokens without replacing them, asks for a dedicated bot token without echo and the exact positive Telegram user ID (the private chat ID is the same), then sends a protected ForceReply synthetic code to that private chat. Open the bot and send `/start` first; reply to the setup message to prove operator access. Forwarded, wrong-user or stale replies are rejected. It never deletes a configured webhook or competes with an already enabled controller's polling.

Only missing ACB username/password/exact-account files are requested, all without echo; an existing encrypted bank session cannot reveal the login password. Files remain outside Git, owner 1000:1000 and mode 0600. Password whitespace is preserved. AI defaults off; choosing AI requires its real base URL, exact vision model and key plus explicit privacy/retention approval. No model or credential is invented.

After owner confirmation (`BAT`), setup validates an isolated candidate environment with Compose and controller `--check-config`. Live flags remain unchanged until preflight passes. It then atomically updates only setup-owned keys and runs `deploy.sh --reconcile <committed-SHA>` as the deployment owner. That mode refuses a changed release or pending deploy; it cannot redeploy an old worker. A running controller is fenced by mutation admission before any enabled reconfiguration or disabling. An active login blocks recreation rather than canceling an OTP. Activation failure restores the previous environment if it was not concurrently modified, then attempts admitted reconciliation; unresolved failures are explicit, never force-canceled.

Successful setup reports **configured/private reply verified/locally ready**, not successful live ACB recovery. It does not force bank logout or promise current live selectors work: first actual recovery must still demonstrate worker verification, complete catch-up and resumed realtime. Unsupported bank controls stop safely and require the existing manual/app flow. Repeat setup on an already enabled installation checks readiness without re-enrollment, overwriting secrets or disturbing polling; normal pause/status/resume controls are then in Telegram.

## Rollout status and non-negotiable gates

**Leave `AUTH_RECOVERY_ENABLED=false` and `AI_CAPTCHA_ENABLED=false` by default.** Deterministic fixtures and executable local Chromium/scheduler/finalizer/client smokes provide implementation evidence, not real ACB or Telegram acceptance. This document does not claim a successful live recovery, real bot UI validation, a live 9router model preflight, or production Docker rollout. Final aggregate verification evidence is recorded separately after integration checks.

Observed verification environment on 2026-10-02:

- Chrome 151 is available at `/opt/google/chrome/chrome`; the actual-Chromium fixture smoke passed. This proves the exercised local fixture browser path, not the live bank selectors.
- `go test ./... -count=1` passed: 32 packages with tests, four without tests (including the temporary integration harness before removal). The solver timeout fixture now cancels after request arrival rather than racing a 10ms connection deadline.
- `go test -race ./internal/storage ./internal/authrecovery ./internal/challenge ./internal/telegramauth ./internal/authsession ./internal/monitor ./cmd/auth-browser -count=1` passed for all seven packages. This race command did not enable the opt-in fixture; the real Chromium regression above ran separately with `ACB_BROWSER_INTEGRATION=1`. Final shipped controller/browser/worker/gateway/dbtool entrypoints compiled successfully.
- Final pre-delivery `go test -race ./... -count=1` and `go vet ./...` passed (32 tested packages, three with no tests after removing temporary harness commands). The actual Chromium opt-in regression also passed again. Review fixes preserve fresh-attempt login while preventing same-attempt credential replay, disposition rejected onboarding callbacks without blocking later polling, and fence enabled-controller reconfiguration against active authentication.
- One-command setup: 14 deterministic Python regressions passed; a real PTY smoke exercised interactive prompts with getpass echo disabled, password whitespace preserved, isolated preflight before activation, and injected upstream/deployment fixtures. The actual stable-symlink wrapper `--help` passed; both workflow YAML files and 32 shell run blocks parsed/passed bash syntax, plus changed shell scripts passed ShellCheck error checks. These local checks are not Docker/VPS/live-bot evidence.
- The combined real-process harness passed: actual Chromium browser child, independently killed/restarted controller child, production worker verifier/RPC/Monitor scheduler and fixture bank/Bot API/9router HTTP endpoints. One login, one leading-zero OTP, one refused OCR request followed by human fallback; two transactions, one missed-credit event and one delivery. Controller death did not release the automatic gate; worker catch-up did, then realtime resumed.
- Fault scenarios passed: `WAITING_OTP` restart preserved the prompt; wrong-user/forwarded/stale replies were rejected; unavailable verifier stayed `VERIFYING`; SIGKILL after durable `CONSUMING` did not submit/replay OTP and required operator intervention; browser process loss/restart retained attempt ordinals and rejected the old prompt. Persisted metadata and outbound payloads were checked for synthetic-secret leakage. Throwaway harness files/binaries were removed after execution.
- Real Chromium regression `ACB_BROWSER_INTEGRATION=1 BROWSER_BIN=/opt/google/chrome/chrome go test ./cmd/auth-browser -run '^TestBrowserAutomationInitialCaptchaAndOTP$' -count=1 -v` passed, including unsafe controls, frame/origin checks and missing exact account. It exposed and verified a fix: an exact-account selection in progress is `ACCOUNT_SELECTION_PENDING`, not a terminal missing-account error; verification resumes in the same attempt.
- Actual standalone controller lifecycle smoke passed: local readiness with Telegram degraded, second-instance lock rejection, clean SIGTERM, existing session untouched. Disabled preflight performs no integration I/O. Deployment shell syntax and `shellcheck -S error` passed; actual release-flag parser covered default-off, human/AI, strict boolean, missing AI override and legacy exclusion. YAML topology checks are static, **not Docker Compose/deploy evidence**.
- `docker` is not available on PATH, and neither `bundle/images.env` nor `deploy/.env.production` exists in this checkout. Production Compose interpolation, image build/deploy, lifecycle/fault rehearsals and rollback therefore have no observed acceptance evidence here.
- Filename-only inspection found existing `deploy/secrets/worker_internal_token`, `tts_internal_token`, `bark_basic_auth_user`, `bark_basic_auth_password` and `app_master_key`. It did **not** find `acb_username`, `acb_password`, `acb_account`, `telegram_bot_token`, `auth_browser_internal_token` or `ninerouter_api_key`. Secret contents were not read or validated; a filename is not credential-validity evidence.
- No real private-bot/operator interaction, owner-validated live ACB login/CAPTCHA/OTP DOM, configured live vision-model/privacy-logging verification or approved maintenance window is available. Human and AI rollout gates remain unmet.

Production prerequisites not supplied by repository fixtures:

1. Account-owner access to current live ACB login/CAPTCHA/OTP DOM via the existing manual auth/noVNC path. Validate sanitized type/name/id, label, form action, error codes and exact account/history selection. Current fixture selectors are not a verified live ACB contract. Do not retain raw HTML, cookies, tokens or field values as evidence.
2. ACB username/password/exact account secret files, correct existing master key and internal browser/worker tokens.
3. A dedicated Telegram bot token, independently verified exact private operator chat/user IDs, and the operator's `/start` interaction. A real private-chat synthetic challenge UI check is still required.
4. For AI: operator-controlled 9router instance/base URL, explicit non-combo vision model ID and API key, data-URI support and payload privacy/retention checks at router/proxy/upstream. Synthetic OCR success does not prove bank CAPTCHA accuracy.
5. An operator-selected maintenance window and permission to exercise controlled bank reauthentication. Do not force logout a production account actively receiving payments outside that window.
6. A real release bundle and pipeline-built digest-pinned worker image containing the controller, with Docker-capable deployment/rehearsal access. Local fixture smokes do not validate production Compose/rollback.

If any human-mode gate is missing, keep recovery off. If only the AI gates are missing, validated human mode can operate with AI off; AI outage must not disable human CAPTCHA/OTP handling. SafeKey push, QR, biometric or device binding that cannot use text OTP must use the app/manual flow; never claim automated success or invent an OTP button.

## Exact configuration

Configuration is loaded only by the controller; gateway/worker do not require bot/bank credentials. Disabled recovery returns before integration secrets or external I/O. Flags accept exactly `true` / `false` (unset means false), not `1` / `0`. `.env.example` contains non-secret defaults and empty ID/model placeholders; production configuration belongs in `deploy/.env.production` with the release's runtime digest file kept separate.

| Field | Default / required value |
| --- | --- |
| `AUTH_RECOVERY_ENABLED` | `false`; true admits the optional service/profile only after the gates above. |
| `AI_CAPTCHA_ENABLED` | `false`; true enables one crop-only OCR request per browser attempt. |
| `DATABASE_PATH` | `/data/gateway.db`; persistent filesystem path, not in-memory SQLite. |
| `AUTH_BROWSER_URL` | `http://auth-browser:8181`; private internal browser service. |
| `WORKER_RPC_URL` | `http://worker:8190`; worker verifier/scheduler, not a second controller-side bank client. |
| `PUBLIC_ORIGIN` | Existing canonical HTTPS dashboard origin, no query/fragment; manual link is `${PUBLIC_ORIGIN}/admin`. |
| `APP_MASTER_KEY_FILE` | `/run/secrets/app_master_key`; existing valid master key, not a newly generated replacement for an existing DB. |
| `AUTH_BROWSER_INTERNAL_TOKEN_FILE` | `/run/secrets/auth_browser_internal_token`. |
| `WORKER_INTERNAL_TOKEN_FILE` | `/run/secrets/worker_internal_token`. |
| `ACB_USERNAME_FILE` | `/run/secrets/acb_username`. |
| `ACB_PASSWORD_FILE` | `/run/secrets/acb_password`; spaces are significant, only a terminal newline is removed. Files reopen for each attempt so rotation takes effect. |
| `ACB_ACCOUNT_FILE` | `/run/secrets/acb_account`; exact ASCII-digit account number, not masked account display. |
| `TELEGRAM_BOT_TOKEN_FILE` | `/run/secrets/telegram_bot_token`. |
| `TELEGRAM_CHAT_ID` | Required exact nonzero signed int64 for the private operator chat; no example real ID. |
| `TELEGRAM_USER_ID` | Required exact nonzero signed int64 for the operator; no username allowlist or enrollment. |
| `AUTH_RECOVERY_CAPTCHA_TTL_SECONDS` | `180`; valid 30–180, further bounded by browser/session expiry. |
| `AUTH_RECOVERY_OTP_TTL_SECONDS` | `120`; valid 30–120, further bounded by browser/session expiry. |
| `NINEROUTER_BASE_URL` | Required only when AI is true; configured HTTPS endpoint or deliberately private Docker service hostname over HTTP, normalized to `/v1` once. No userinfo/query/fragment or redirects. |
| `NINEROUTER_CAPTCHA_MODEL` | Required only when AI is true; exact operator-chosen vision model, never inferred from the coding assistant or auto-selected combo. |
| `NINEROUTER_API_KEY_FILE` | `/run/secrets/ninerouter_api_key`; mounted/read only when AI is true. |

Do not use retired keys such as `AI_PAGE_CLASSIFIER_ENABLED`, plural Telegram allowlists, a shared challenge TTL, or configurable attempt caps. Fixed limits: **3 browser attempts per outage-episode budget, 3 CAPTCHA submissions per attempt including AI, 1 OTP submission, 1 AI request**. Browser TTL is 15 minutes; login submission cooldown is at least 60 seconds. Internal generation changes and restart do not reset the episode budget; only a fresh operator confirmation grants a new budget while preserving the monotonically increasing attempt ordinal.

Production secrets must use `_FILE`; inline bank credentials are not supported. Do not put tokens/passwords in `.env`, Docker image layers, command arguments, logs, config dumps, Git or chat. No `TELEGRAM_BASE_URL` / bank-host / fixture bypass environment variable exists; local harness transport/DOM injection does not relax production TLS/origin checks.

## Bot creation and file provisioning

1. Create a **new dedicated bot** through the verified Telegram BotFather account. Do not reuse a notification bot with an existing webhook or another polling consumer.
2. The sole operator opens the bot's **private chat** and sends `/start` once. This enables messaging; it does not auto-enroll, change the configured allowlist or authorize other users. Verify the numeric chat and user IDs from a trusted Telegram/Bot API source; never use username matching, groups or channels.
3. Provision credentials out of band via a secure VPS/secret-management channel, **not by sending them to this bot or any chat**. In the existing deployment layout, host files are under `${DEPLOY_PATH}/deploy/secrets`; Compose mounts them at the `_FILE` paths above. Do not send passwords, bot/router keys, master key or bank session cookies in Telegram.
4. Set the secrets directory to **0700, owner 1000:1000**, and each controller secret file to **0600, owner 1000:1000**. The controller runs as UID/GID 1000. Set the protected production environment file according to existing deployment permissions (0600, owner 1000:1000). These settings must already be correct at deployment; do not rely on deploy silently changing live secrets.
5. Required human-mode files are the existing master key/browser token/worker token plus `acb_username`, `acb_password`, `acb_account`, `telegram_bot_token`. Provision `ninerouter_api_key` only for AI mode. Backup provisioned/enabled secrets using the existing encrypted secret-backup procedure; keep backup decryption identities off the VPS.
6. Preserve the password exactly, including significant leading/trailing spaces; account file must contain the exact number. Rotate through files, keeping ownership/mode. A decryption failure is a manual master-key incident, not a reason to repeatedly login or replace the key.

Telegram bot chats are **not end-to-end encrypted**. Only input **ACB login OTP for the specific prompted login**, never a transfer/payment OTP. Prompt/accepted response deletion is best effort and cannot erase Telegram server/notification history. `protect_content` is not an E2EE promise. Redacted evidence must not include real OTP/CAPTCHA/password/cookies or model payloads.

## Runtime, deployment and readiness

- `recovery-controller` uses `${WORKER_IMAGE_REF}` with entrypoint `/recovery-controller`; `/worker` remains the worker image's default entrypoint. No new image key/build matrix or legacy runtime-format entry is required.
- Production service is an optional `auth-recovery` profile singleton named `acb-recovery-controller`, using the shared SQLite/WAL volume and internal `acb-core` + outbound `acb-egress` networks. It is non-root, read-only, drops capabilities, uses no-new-privileges and bounded resources, with no host port, edge exposure or Docker socket. Browser/CDP remains private.
- Human mode uses `deploy/compose.prod.yaml` without requiring an AI key. AI mode additionally uses `deploy/compose.auth-recovery-ai.yaml`; packaging/checksums must include that override. The release helper selects profile/override according to strict flags and target-bundle support, not a root-directory Compose invocation.
- The controller uses **`storage.OpenRuntime` and does not migrate**. Apply schema v12 (`012_auth_recovery.sql`) via the existing dbtool/deploy migration pipeline before starting it; do not edit applied migration checksums or start a controller against old schema.
- Acquire `DATABASE_PATH + ".auth-recovery.lock"` before polling Telegram/opening browser; hold through shutdown. A second controller exits instead of competing for updates. `--check-config` intentionally does not take the runtime singleton lock.
- Deploy first acquires the DB mutation admission gate. Active auth blocks deploy with an instruction to wait or use `/acb_pause`; never force-cancel a live operator OTP for deployment. Once admission is locked, stop controller before worker/browser replacement/migration; start it only after worker/browser health and release admission before it starts new logins.
- Flag-off or rollback to a legacy bundle without the service/binary must stop the new controller; do not leave it paired with an older worker or invoke nonexistent old profile/override services. Release digests/runtime files remain immutable.
- SIGTERM stops new polling/actions without blindly finishing an existing attempt. Persisted attempt/browser metadata is reconciled after restart; controller restart does not stop worker/gateway. Shutdown releases its lock last.

**Readiness is not external credential validation:** localhost `http://127.0.0.1:8182/readyz` is internal only; `/recovery-controller --readiness-check` probes it. It checks local initialized runtime/DB/schema/singleton/config, not successful ACB login. Telegram/AI connectivity degradation is reported separately rather than failing the entire deployment. A locally ready controller with an unavailable bot must not initiate a login requiring undeliverable prompts.

**Preflight is separate:** `/recovery-controller --check-config` reads configured secrets safely, checks Telegram `getMe`, `getWebhookInfo` and the configured private destination, and when AI is enabled checks exact model discovery plus synthetic `AB12CD` PNG OCR. Configured user ID authorization still requires the actual operator UI/reply exercise. Webhook conflict is `TELEGRAM_WEBHOOK_CONFLICT`; the controller never deletes that webhook. Preflight does not acquire the runtime lock, call `getUpdates`, open an ACB browser, or submit a bank CAPTCHA/OTP.

Run operational commands from the **real release context**, not `docker compose -f compose.prod.yaml` at repository root. After sourcing `deploy/simple-lib.sh`, with `DEPLOY_PATH`, `release` and `runtime` pointing to the deployment's actual release/runtime files:

```bash
compose_release "$release" "$runtime" config --quiet
compose_release "$release" "$runtime" run --rm --no-deps recovery-controller --check-config
```

The profile is only selected when `AUTH_RECOVERY_ENABLED=true`; an intentionally disabled installation does not need integration secrets or this command. For preflight, enable the flag in the protected release configuration only after provisioning; `run --no-deps` performs checks without starting worker/browser/controller polling. Do not invent image digests or rewrite old release bundles to make a command pass. Network access to Telegram and the configured router must be available. If this installation actually uses `HTTPS_PROXY`, allow the exact configured hostnames in that proxy's policy; do not guess a router domain or weaken unrelated egress policy.

## Controlled rollout: human first, then AI

### 1. Human CAPTCHA stage

Complete live DOM and secret/bot/window prerequisites, apply migration with the existing deployment pipeline, and configure:

```dotenv
AUTH_RECOVERY_ENABLED=true
AI_CAPTCHA_ENABLED=false
```

Keep actual credentials in files and exact operator IDs in protected configuration. Validate release interpolation and run `--check-config`; this stage does not read/mount an AI key. Verify a real private-chat **synthetic** CAPTCHA photo and OTP prompt: ForceReply targets the correct message, expiry is clear, stale button/reply is rejected, and synthetic answer cleanup is best effort. Avoid real secret values in UI evidence.

In the agreed maintenance window, observe one controlled ACB human-CAPTCHA recovery. The operator must complete prompted CAPTCHA and login OTP without interacting with VPS/noVNC mid-flow; exact account/history selection, worker verifier success, encrypted persistence, all required catch-up days and gate release must be observed. Manual/noVNC remains available if controls are unsupported. Do not mark this stage accepted based only on local fixtures.

### 2. AI preflight and stage

Before any **bank** crop leaves the controller, verify router/proxy/upstream request/response logging and retention. `ENABLE_REQUEST_LOGS=false` in 9router disables one logging layer only. The controller must send only the isolated PNG crop; no full-page fallback, DOM, cookies, account, balances, transactions, username/password or OTP may enter the AI request.

Provision the API key and choose a specific vision model/base URL. Then enable `AI_CAPTCHA_ENABLED=true`, validate the merged release AI override and run `--check-config` again. Discovery uses `/v1/models/image-to-text` with `/v1/models` only on 404; synthetic `AB12CD` must be read correctly. Treat this as a wire-contract check, not a measured ACB CAPTCHA success rate.

In a controlled recovery, verify at most one AI call and one AI answer submission per browser attempt. A valid answer is never sent to Telegram. AI refusal, malformed response, quota/down/timeout or bank rejection must fall back to a fresh human CAPTCHA prompt in the same live browser attempt, within the three-submission cap; OTP always remains human. Do not provoke repeated incorrect bank OTP/CAPTCHA on the real account—use fixtures for fault paths. If AI prerequisites fail, turn AI off and retain the accepted human flow; do not change model/provider automatically.

## Operator commands and reply discipline

| Command | Effect |
| --- | --- |
| `/start`, `/help` | Help only; no enrollment/permission changes. |
| `/acb_status` | Short connection/episode IDs, state/budget, current challenge expiry, pause, catch-up range/progress and transport degradation; no bank secrets, full account or balance. |
| `/acb_login` | One-shot confirmation for configured `AUTH_REQUIRED` or initial configured `UNCONFIGURED`; active healthy session returns “Phiên đang hoạt động”, no forced logout. Manual attempt is waited for. |
| `/acb_retry` | One-shot 60-second confirmation. Rearm allowed waiting/manual/retry login states with a new episode budget and persisted 60-second submission cooldown. An active valid prompt is not bypassed. If only catch-up failed with a valid verified session, retry the same run/progress, **not login/OTP**. |
| `/acb_pause` | Durable pause; cancel only linked uncommitted automatic login and invalidate prompts. Does not logout verified session, stop normal worker monitoring or cancel committed catch-up. |
| `/acb_resume` | Clears pause; **does not reset exhausted budget or manual-required circuit**. |
| `/acb_cancel` | One-shot confirmation to cancel an uncommitted episode and suppress automatic recreation. Cannot remove a committed catch-up gate/session. |
| `/acb_manual` | One-shot confirmation; pause automatic login, cancel its uncommitted prompts and return `${PUBLIC_ORIGIN}/admin`. No direct privileged noVNC URL; committed catch-up continues. |

Only **directly reply to the current CAPTCHA image or OTP prompt**. Do not type a challenge ID or submit a forwarded/edited/via-bot/media message. Exact private chat/user and persisted prompt ID, generation, attempt, browser revision and TTL all must match. Edge whitespace is trimmed; OTP leading zeroes and CAPTCHA case are retained. Format checks require OTP ASCII digits 4–10 and CAPTCHA ASCII letters/digits 1–16, further restricted by the form.

A message becomes answerable only after its ID persists as `PENDING`. Reply consumption is atomic before action; duplicate/late replies cannot submit twice. Timeout or crash after `CONSUMING` does **not** replay stored input—there is no stored plaintext. Browser re-observation may continue if authentication already advanced, otherwise the controller asks for operator intervention. Ordinary restart can retain a still-valid pending prompt; revision change/browser loss invalidates it.

Notice delivery is **at least once**. Lifecycle keys deduplicate logical events, but crash after Telegram send before DB persistence can cause duplicate messages. Neither Telegram notices nor outgoing transaction webhook networks promise exactly-once delivery; semantic transaction/journal/event deduplication is the relevant data invariant.

## What “recovered” means

1. Browser passes existing authenticated/history predicates for the exact configured account.
2. Shared `authsession.Finalizer.Complete` encrypts with connection/generation AAD and asks the **worker RPC verifier** to validate it; failure cannot replace the old session/checkpoint or announce recovery.
3. One DB transaction commits the session, `MONITORING` connection, run intent and automatic `CATCHING_UP` gate. The connection enum alone is **not** proof realtime resumed.
4. Worker catch-up scans every frozen required day using normal per-day pagination/retry/dedup. Automatic outages beyond seven days, including ten days, are not silently clipped. Manual/startup/onboarding recovery retains existing seven-day semantics and **manual/payment-boost behavior is unchanged**.
5. The automatic-only gate blocks realtime/payment boost, keepalive, sync admission and queued history before bank I/O. It remains on failed/canceled run or controller downtime. Catch-up/verifier remain allowed; history work is requeued rather than failed solely because of the gate.
6. Worker completion validates durable coverage. Crossing midnight or a stopped-worker gap creates a same-episode extension from the day after the previous required end through current Asia/Ho_Chi_Minh date. The old range is immutable, the gate stays held until all tail days are complete, and release does not depend on Telegram being online.
7. Only then enqueue “Đã khôi phục ACB. Đã bù giao dịch và tiếp tục theo dõi.” If monitoring schedule is disabled, say the session is ready but the schedule is paused—not that polling resumed. Controller pause prevents future automatic logins, not existing verified worker monitoring.

Frozen range starts at the earliest valid checkpoint coverage end, episode creation day or today minus six days. Corrupt/future checkpoint yields `INVALID_CHECKPOINT` and requires controlled admin/data correction; retry cannot erase that problem. Required range unavailable upstream yields `HISTORY_RANGE_UNAVAILABLE` and retains the gate. A non-auth catch-up retry retains run identity/range/checkpoint/progress and session; auth loss during catch-up returns to the **same episode budget** across the new generation.

## Failure triage

| Observation | Operator action / invariant |
| --- | --- |
| Bot auth/webhook failure | Check token/private destination and remove a conflicting webhook only through a deliberate owner-controlled operation; controller never auto-deletes it. Worker/gateway remain alive. |
| AI degraded/refused/timeout | Reply to human CAPTCHA prompt; no automatic second OCR that attempt. Turn AI off if instance/privacy/model gate is unverified. |
| CAPTCHA cap exhausted | Inspect `/acb_status`, use confirmed retry when appropriate; do not reset generation to evade cap. |
| OTP expired/incorrect/unanswered/unknown outcome | `WAIT_OPERATOR`, no resend or answer replay; use confirmed retry after cooldown or manual flow. |
| Credentials rejected/account locked | Stop automatic login; inspect credentials/bank state securely and use manual. Do not repeatedly provoke bank lockout. Ambiguous page errors are not guessed. |
| Browser/network failure before login | Persisted 30s/120s jittered retry within three-attempt budget. A browser conflict does not cancel someone else's manual session. |
| Maintenance | Persisted 15-minute wait within the same cap; `/acb_resume` cannot replenish attempts. |
| Worker verification unavailable | Stay `VERIFYING` within browser TTL; do not generate new OTP solely for an RPC outage. |
| Missing/invalid local session | Durable `AUTH_REQUIRED` recovery preserves journal/checkpoint. Decryption failure is `MANUAL_REQUIRED`; check master key, do not replace it blindly. |
| DB unavailable or stale generation | No new cached-generation browser side effect; resolve DB health or superseded attempt. |
| Verified but catch-up failed | Gate remains. Confirm `/acb_retry` to retry catch-up with same session when non-auth/retryable; manual correction for invalid checkpoint. |
| Deploy blocked by active auth | Wait for completion/expiry or deliberately `/acb_pause`; never force-cancel operator input to unblock release. |

## Acceptance evidence: fixture versus live

**Deterministic/offline evidence** exercises persisted budgets across generation/restart, manual/deploy admission, duplicate/replayed/late replies, real Chromium fixture initial CAPTCHA + leading-zero OTP, wrong/ambiguous controls, OCR fault fallback/redaction, shared finalizer crash boundaries, worker catch-up/automatic gate, long gaps/midnight and unchanged manual boost. HTTP bank/Bot API fixtures are explicit upstream substitutes. Actual Chromium and worker scheduler paths strengthen this evidence; they still do not establish compatibility with current live ACB selectors or prove Telegram UI/provider privacy. Final suite/harness outcomes must be reported only after they are observed, not inferred from test names or mock echoes.

Observed combined harness output:

```text
PASS full recovery: browser_attempts=1 login_submits=1 otp_submits=1 ai_calls=1 transactions=2 events=1 deliveries=1
PASS SIGKILL after durable CONSUMING: otp_submits=0 state=WAIT_OPERATOR old_prompt=INVALIDATED replay_rejected=true
PASS actual browser SIGKILL/restart: attempt_count=2 ai_calls=2 old_prompt_rejected=true durable_retry_budget=true
PASS reachable full smoke complete; LIVE_ACB_PRIVATE_TELEGRAM_9ROUTER_ACCEPTANCE=UNVERIFIED
```

**Live acceptance remains unaccomplished here.** Record redacted observable evidence for each gate before enabling unattended production:

- Real allowed private chat: status, pause/resume, ForceReply synthetic photo/text, stale callbacks, best-effort deletion; no secrets in screenshots.
- Current account-owner DOM and exact account handoff; human-mode controlled recovery succeeds end to end without mid-flow VPS interaction.
- Live configured 9router discovery/synthetic image passes; privacy controls checked; controlled bank AI crop and human fallback verified without OTP/credentials in model request.
- Durable `AUTH_REQUIRED` yields episode/notice within ten seconds (or pending notice during Telegram outage), worker verifier passes, every required catch-up day completes, first realtime follows gate release, missed transactions are present once semantically in journal/events.
- Controller restart/pause/circuit persists without stopping gateway/worker; old prompts cannot submit; worker outage cannot produce a restored notice; admin manual rescue still works.
- Pipeline image/bundle production interpolation and lifecycle/fault rehearsals validate human flags, AI override/key conditionality, same worker digest, disabling recovery and rollback to a legacy bundle without an orphan controller.

Until those observations exist, label results **implemented / deterministic fixture-tested**, identify the exact missing live prerequisite, and keep the corresponding production flag off. Do not convert a synthetic/mock success into a claim that a bank account, real Telegram UI or Docker deployment has been accepted.
