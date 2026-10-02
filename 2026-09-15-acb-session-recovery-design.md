# ACB Telegram Session Control Design

> Updated 2026-10-03. This is the implemented architecture after the Telegram control cutover. The companion [operations and acceptance runbook](2026-09-15-acb-session-recovery.md) contains commands and observed verification. Supported automation starts only after authenticated one-use button consent; historical automatic-start/retry behavior is no longer the contract. Fixture success is not live bank/provider/Telegram/deployment acceptance.

## Goal, retained behavior and limits

Manage ACB session status, login, stored username/password and logout through the authorized private Telegram operator. After one login button, recognized CAPTCHA/login/verification/catch-up steps are automatic; **login OTP from the ACB app remains human**. Username/password changes use a separate OWNER-protected HTTPS page, not chat. The tracked bank account is immutable after initial setup/import.

- No new login from session loss, prior operational history, startup, restart, cron, text commands, status requests or unlocking recovery. Session loss creates a warning and waits for a button.
- No dashboard/manual bank login, browser iframe, screenshot screen proxy or VNC control remains. Unsupported SafeKey/push/QR/biometric/device binding must be resolved in the bank app or with bank support, not guessed by AI.
- Controller does not verify bank sessions or scan history; worker remains the owner of those HTTP operations, scheduling, transaction deduplication, journal/checkpoints and catch-up completion. QR, tracking configuration, history and data sync remain outside login control.
- AI receives only an established safe CAPTCHA crop, never credentials, OTP, cookies, DOM, account, balance, history or handoff. It is not a page classifier or autonomous browser agent.
- Missing credentials allow the bot's degraded setup/import menu but not LOGIN or an empty-connection credential grant. Credential decrypt/DB failure is fail-closed, without password-file fallback.
- Unknown outcomes are not retried as if they failed. A time/retry state can allow a fresh button, never grant a new unattended attempt.

## Components and flow

```text
worker detects confirmed session loss
  -> durable AUTH_REQUIRED + recovery episode/warning
  -> private Telegram menu (read-only until consent)
operator presses authenticated LOGIN/RETRY button
  -> transactional one-use consent + admission
  -> recovery-controller -> private auth-browser / Chromium
       safe CAPTCHA crop -> configured AI or human prompt
       login OTP -> current private ForceReply
  -> authsession.Finalizer -> worker verification
  -> encrypted session + durable catch-up intent + recovery gate
  -> worker day-by-day coverage -> gate release -> ready notice

Telegram credential confirmation -> hashed five-minute grant
  -> HTTPS OWNER + CSRF + exact Origin -> encrypted revisioned save
  -> no login until another Telegram LOGIN button

Telegram logout confirmation -> durable generation fence + logout job
  -> worker memory invalidation + one bank DOM revocation/probe
  -> independent local acknowledgement and bank result -> durable notice
```

| Component | Ownership |
| --- | --- |
| `cmd/recovery-controller`, `internal/authrecovery` | Independent singleton using `storage.OpenRuntime`, recovery reconciliation and serialized bank/browser operations; credentials read from encrypted DB only after consent. Five-second safety tick plus wake channel; active observation approximately every 1.5 seconds. |
| `internal/telegramauth` | One authenticated private-chat long-poll consumer, menu/actions/grants, durable offsets, outbox and one separate notice/progress/challenge delivery loop. Callback acknowledgement precedes slow work. |
| `internal/challenge.Broker` | Durable challenge creation and delivery, prompt/revision/expiry correlation and reserve-before-submit reply disposition. Answers/images stay in memory. |
| `cmd/auth-browser`, `internal/authbrowser` | Token-protected Chromium/CDP login automation and revoke-only DOM operation. No publicly routed browser/CDP/control API. Chromium/display lifecycle remains; remote desktop is removed. |
| `internal/captchasolver` | Exact configured vision model, bounded crop-only OCR and finite safe diagnostics. No provider/model auto-selection. |
| `internal/authsession.Finalizer` | Generation-bound encrypted handoff, worker verification, atomic session/catch-up commit and idempotent cleanup/scheduling. Worker RPC receives encrypted session, not bank password. |
| Worker / `internal/monitor` / `internal/acb` | Bank HTTP verification, monitoring, catch-up and history. Shared session-operation fences and memory clear prevent stale-session resurrection. |
| `internal/storage`, `internal/httpapi` | Transactional consent/action/grant/save/logout admission and encrypted credential storage; private credential HTTP endpoints reuse OWNER/CSRF protections. |
| Dashboard | Sanitized read-only recovery status plus existing tracking/QR/history/sync; distinct credential page outside AdminLayout in private admin routes. |

No DB transaction spans browser, Telegram, AI or worker RPC I/O. Durable admission/reservation precedes external effects, and generation/revision/expiry checks prevent stale commits. This is not a claim that DB and network I/O are one atomic transaction.

## Schema, consent and admission

Migration **013** (`013_telegram_session_control.sql`, `TelegramSessionControlSQL`, checksum **`2026-10-02-v13-telegram-session-control`**) extends existing migration-012 recovery/action/challenge tables and adds encrypted credentials, credential grants, per-revision AI claims and logout jobs. Migration 012 and its checksum remain unchanged. IDs/timestamps/counters and safe state/reason metadata are persisted; raw updates, plaintext answers/crops/credentials and raw grant tokens are not.

One open episode and one active attempt per connection remain transactional invariants. `ConsumeTelegramAuthAction` validates bot/chat/user/message, action TTL/status, generation and configuration revision in the same transaction as its effect. LOGIN/RETRY for login sets `consent_action_id`, `consent_expires_at` (60 seconds) and unset `consent_consumed_at`; it also binds the consumed action to the selected/created episode in that transaction. Catch-up RETRY never grants login consent.

`StartRecoveryAuthAttempt` requires the actual consumed action, correct episode/identity/revisions, unexpired and unused consent. It consumes that consent, snapshots the current credential revision and creates the new attempt/generation atomically. The action is checked against the pre-bump generation, not incorrectly required to match the newly created attempt generation. `RecordRecoveryLogin`, submission reservations and attempt checks validate the consumed consent; direct storage calls cannot bypass the gate or grant authority using a reason string such as `OPERATOR_CONFIRMED`.

All action/grant/save/logout mutation transactions call `checkMutationAllowedTx` within that transaction, not only an earlier process-level check. Deployment/restore admission blocks new mutations while read-only menu/status work. External configuration/generation changes supersede old actions/challenges/grants. Coordinator does not start a browser, decrypt credentials or call AI before consent.

| Bound | Contract |
| --- | --- |
| Consent | One browser attempt per authenticated button; pending admission expires after 60 seconds. |
| Browser attempt | TTL 15 minutes. |
| Login cooldown | At least 60 seconds between login submissions, durable across restart. |
| CAPTCHA | At most 3 submissions per attempt, including AI answers. |
| AI | At most 3 calls per attempt, at most one call for each distinct browser CAPTCHA revision. |
| OTP | At most 1 submission per attempt; 4–10 ASCII digits, at most 120 seconds or shorter browser expiry. |

Episode flow is `DETECTED -> STARTING -> LOGIN -> WAITING_CAPTCHA / WAITING_OTP -> VERIFYING -> CATCHING_UP -> COMPLETED`, with waiting states such as `WAIT_OPERATOR`, `RETRY_WAIT`, `MAINTENANCE_WAIT`, `MANUAL_REQUIRED` and terminal `SUPERSEDED`/`CANCELLED`. State names do not confer permission. Browser failure, maintenance, credentials/OTP rejection, timeout, challenge expiry, unsupported controls or uncertain submit outcome require fresh operator consent for any new attempt. Unlocking login does not grant that consent.

Confirmed auth loss during polling/catch-up invalidates prior consent/challenges and warns the operator. Prior operational evidence distinguishes outage wording from onboarding only. Catch-up retry with a verified session resets the existing retryable run, not login/OTP.

Migration cancels legacy unfinished unverified attempts and invalidates their actions/challenges, leaving them waiting for new consent. Healthy monitoring sessions and already committed verification/catch-up remain. Restart may observe/verify/continue committed work from an existing consented attempt; it cannot replay Start, credentials or OTP across an unknown outcome. Consumed input is never reset to pending merely because a process restarted.

## Telegram authorization and delivery

`/start`, `/menu`, `/help` render the control panel. Runtime registers private-chat commands `menu`, `acb_status`, `help` after bot config checks; `--check-config` does not register commands. Login/retry/cancel text shortcuts create confirmation buttons rather than bank side effects. Pause/resume are labeled “Khóa đăng nhập” / “Mở khóa đăng nhập”; neither is bank logout or login consent.

Navigation callbacks `nav:menu`, `nav:status`, `nav:help`, `nav:credentials`, `nav:logout` require the same exact numeric private operator before dispatch. One-use bank/credential actions use random `ar:<id>` tokens bound to their originating message, bot/chat/user, generation/configuration and 60-second TTL. CAPTCHA-image actions additionally bind attempt/revision. Callback payloads contain no credentials, OTP, cookies or account. Wrong user/chat, group, forwarded/edited/inline or stale/duplicate input cannot create an attempt, grant or logout.

Authorized callbacks are acknowledged immediately using a separate bounded two-second timeout before longer processing. Menu/status read DB without bank HTTP; callback response goals do not promise bank latency. Poll offsets advance only after durable disposition, including safe rejection. There is one long-poll consumer; an existing webhook is refused with `TELEGRAM_WEBHOOK_CONFLICT`, never deleted automatically.

Coordinator commits then calls `Wake()`; a separate single delivery loop handles notices/progress and `Broker.DeliverPending`, using a buffered wake plus one-second tick. Telegram I/O never holds the coordinator's bank-operation mutex. One episode progress message is CAS-bound by generation and edited at most once/second/chat, coalescing observed states. Prompt/terminal messages take priority; transport respects retry-after. `TELEGRAM_MESSAGE_NOT_MODIFIED` is a successful no-op; `TELEGRAM_MESSAGE_UNEDITABLE` permits a replacement. A timeout alone does not justify another status message. Failed CAS of a newly sent message causes best-effort deletion.

The session-loss notice explicitly says the system has **not logged in again** and offers LOGIN/status/credential changes. Credential rejection offers a credential link, not an automatic password retry. Stale outbox progress coalesces to current state instead of replaying obsolete steps after bot reconnection. Delivery is at least once: a crash between sending and DB acknowledgement can duplicate a notice, never duplicate its bank authorization.

## CAPTCHA/OTP safety and browser behavior

Browser actions use opaque current revisions, serialized observation/action/handoff work, unique visible enabled controls, checked same-origin form/action and attempt expiry. Consuming a revision before clicking prevents replay after timeout. Initial CAPTCHA is solved before the single credential submit; later CAPTCHA uses the appropriate challenge operation. Handoff requires recognized authenticated/history signals and the exact immutable account, never the first/masked account.

`auth_recovery_ai_claims` has primary key `(attempt_id,browser_revision)`. `ClaimRecoveryAI` checks consent/fences and durably inserts the claim/increments `ai_used` before I/O. A→B→A cannot call AI for A again, and restart does not replenish the three-call budget. New attempts reset their own AI budget only after new consent. Re-submitting a credential form requires explicit `CAPTCHA_REJECTED`/`INVALID_CAPTCHA` plus fresh revision; image replacement alone leaves `LOGIN_OUTCOME_UNKNOWN`, not a guessed retry.

`Broker.Prompt` only validates and creates `DELIVERING`; it performs no Telegram send and retains no crop. `DeliverPending` re-observes revision/fence/TTL, captures a safe current crop in RAM when applicable, sends, then CAS-binds the returned prompt as `PENDING`. A failed CAS deletes the new prompt best effort. Unknown delivery outcome invalidates that challenge rather than adopting/replaying an unbound prompt; retry respects transport limits and original expiry. Startup invalidates unbound `DELIVERING`; valid bound `PENDING` can be revalidated.

`HandleReply` identifies the challenge solely through the exact reply-to prompt. Before submission it validates private identity, attempt/generation/revision/expiry and atomically reserves `CONSUMING`. OTP preserves leading zeroes; no transfer OTP is requested. Reply deletion occurs after durable rejection/invalidation/consumption. If browser observation fails before submission, persist invalidation before acknowledging that input; if DB disposition fails, retain offset/input for recovery. After reserve or an unknown submit, never replay plaintext.

“Xem ảnh captcha” rechecks the current attempt/revision, captures only a safe image/canvas/bounded rectangle of at most 512 KiB and does not create another challenge or bank submission. If already processed, return that state. No OTP image or full-page fallback is permitted; `UNSAFE_CAPTCHA_CROP` fails closed. Images and answers are not persisted and are protected/deleted best effort. Telegram is not E2EE, and neither deletion nor dropping Go/Chromium references guarantees erasure or zeroization.

## Authoritative encrypted credentials and HTTPS grants

`acb_credentials` stores connection ID, revision, AES-GCM envelope, key ID and timestamp. Plaintext `{"username":"…","password":"…","accountNumber":"…"}` is encrypted using `CredentialsAAD(connectionID,revision)`, literal `acb-credentials:<connectionID>:<revision>`, distinct from session AAD. Runtime reads with `ReadACBCredentials` and compares the admission snapshot revision before credential submit. No file fallback after missing row, DB error or decrypt failure; finite errors include `CREDENTIALS_NOT_CONFIGURED`, `CREDENTIALS_DECRYPT_FAILED`, `CREDENTIALS_REVISION_CONFLICT`.

The local-only `recovery-controller --import-credentials` uses the existing singleton lock, master key/DB configuration and three readonly `/run/import` files. Import is insert-only and deployment-admitted, never bank I/O or polling. An existing record is not overwritten. A current decryptable session must match exact `Handoff.Fields["AccountNbr"]`; otherwise `CREDENTIAL_ACCOUNT_MISMATCH`. An unverified initial account is stored for later exact-account login verification, not announced as validated. Regular/no-symlink owner/mode checks are importer prerequisites, not runtime password dependencies.

UPDATE_CREDENTIALS confirmation atomically consumes its Telegram action and creates one pending grant bound to bot/chat/user, connection generation/config revision and credential revision. The token is 32 cryptographically random bytes, base64url; DB keeps only its SHA-256 hash. TTL is five minutes; a new grant revokes the previous pending grant. Raw tokens are not stored in the outbox; a crash before link delivery requires a new confirmation/link.

Link creation uses configured `PUBLIC_ORIGIN + "/admin/acb-credentials#grant=" + token`, never request Host or query parameters, with link preview disabled. The private admin page sits outside AdminLayout, reads the fragment into RAM then clears history via `replaceState`; no local/session storage, query cache, third-party analytics or automatic clipboard copy. Credentials are not prefilled; account is masked and immutable. User-controlled password visibility is local UI only.

| Endpoint | Required boundary and effect |
| --- | --- |
| `POST /api/v1/connection/credentials/grant` with `{"grant":"…"}` | OWNER + CSRF + exact Origin; validate hash/TTL/fences, bind first owner subject atomically, do not consume. Rate limit 10/minute/authenticated owner. Return only revision, masked username/account, expiry, canSave and safe blockedReason. |
| `POST /api/v1/connection/credentials` with grant, expectedRevision, username, password, passwordConfirmation | Same owner/CSRF/Origin; strict JSON/body <=8 KiB, username trimmed <=256 bytes, password <=1024 bytes preserving spaces, equal confirmation, no empty/CR/LF/NUL. Account change is not an accepted field. |

Save checks pending grant/owner/expiry and exact generation/config/credential revisions inside the mutation-gated transaction. `MONITORING`, `AUTH_STARTING`, active attempt/logout/catch-up means `ACB_SESSION_BUSY` (409) with no consumption/write. Link validation is not logout. Successful save encrypts revision+1, bumps config/generation, deletes obsolete persisted sessions, supersedes episodes/challenges/actions, consumes the grant/revokes others and creates a non-consented episode/notice. Return `{"saved":true,"revision":N+1,"requiresLogin":true}`; never validate the password at the bank or start login. Audit records actor/action/connection/revision/result, never secret/token/password hash. Connection ID/account/checkpoints/journal remain.

Expired/invalid/replayed grant is 410; stale revision 409; invalid input 400; OWNER/CSRF errors use existing 401/403; DB/encryption failure is safe 503 with rollback. A timeout after commit does not trigger client auto-submit; the consumed grant prevents another write. Successful UI clears secret state and offers no web login button.

Production protection requires final no-store/no-referrer/frame denial and strict same-origin CSP. `deploy/render-route.sh` emits exact credential path router priority 250, active frontend target, `tunnel-only` and self-contained `acb-credentials-security`; the generic middleware must not overwrite it. Nginx exact path serves SPA without losing headers to fallback redirect. Public viewer continues to deny admin/private API. Access OWNER policy and disabling Cloudflare Rocket Loader/analytics injection on that exact path need actual production verification; generated YAML and sandbox UI cannot prove the edge response. See runbook for the exact CSP and operator commands.

## Logout: durable local fence, independent bank outcome

LOGOUT confirmation consumes its action and creates `acb_logout_jobs` in the same mutation-gated transaction. It captures an encrypted session snapshot with the **session row's actual generation**, optionally the active browser attempt, bumps/fences connection generation, sets `AUTH_REQUIRED`/login paused, invalidates stale work/grants/prompts, cancels recovery/history jobs and deletes the persisted session. It retains credentials, transactions and checkpoints. Revoke decryption uses that stored session generation and supported legacy session AAD, not an assumption that session generation equals the pre-fence connection generation.

Job state and independent evidence are durable: `local_cleared_at` and bank status `PENDING`, `IN_FLIGHT`, `CONFIRMED`, `ALREADY_EXPIRED`, `UNCONFIRMED`. Logout jobs precede recovery under the bank-operation mutex. LOGIN/save remain blocked while local invalidation is pending; bank revocation gets its opportunity before an active browser is cancelled.

Private `POST /rpc/session/invalidate` receives only connection ID/fenced generation, validates current DB/logout fence and clears both monitor/verifier loaders. It is idempotent for the current fence and conflicts for stale input. `acb.Client.ClearSession` empties jar/bootstrap/history diagnostics and closes idle connections. Loader invalidation resets cache and client under one critical section; `SessionRestorer.ClearSession` is required, not an optional assertion.

Every bank request, including bootstrap, keepalive, realtime, catch-up/history pagination and verifier, uses a pinned generation/state/logout check under the shared session-operation mutex. Invalidation takes that same mutex before loader/client clear (lock order operation → loader → client). Cache hits/RestoreEnvelope are guarded too. After local acknowledgement, no old request can start even if a task retains pagination fields. In-flight stale work cannot commit through the generation fence. Verifier checks context/fence in the actual scheduler task and cancels on caller timeout; queued old verification cannot restore cookies after logout.

Worker invalidation has a bounded five-second attempt; bank revocation is not indefinitely delayed by an offline worker. Before the single bank call, commit `IN_FLIGHT`. Private browser `POST /session-revocations` uses the internal token, <=64 KiB body containing operation ID/attempt/handoff, bounded total timeout 60 seconds, and returns only safe status/reason.

Revoke uses a matching active browser or a temporary isolated about:blank/CDP profile with validated official-domain cookies and allowlisted protected URL. It must not run login observers, read credentials, call Start, construct guessed bank operations or post history just to infer logout. Identify one visible unambiguous official-origin logout control and only an explicit logout-bound confirmation. Ambiguous/off-origin/unsupported control fails closed. Navigation and form/request guards prevent unrelated bank actions.

First prove authentication at a read-only protected target and retain the current pre-logout authentication cookies in RAM. After the DOM logout click, probe that protected target in isolation with those **old cookies**, not a cleared post-click jar. Only proven rejection of the formerly authenticated session gives `CONFIRMED`. A clearly expired pre-probe gives `ALREADY_EXPIRED`. Unsupported controls, no safe protected target, no snapshot, unclear/network outcome or still-authenticated probe give `UNCONFIRMED`, never success from local deletion alone. Profiles/contexts and cookie references are discarded after the operation.

| Local evidence | Bank evidence | Durable report |
| --- | --- | --- |
| Clear acknowledged | `CONFIRMED` / `ALREADY_EXPIRED` | `COMPLETED`, describing the bank result separately. |
| Clear acknowledged | `UNCONFIRMED` | `LOCAL_ONLY`; bank revocation remains unestablished. |
| Not acknowledged | Any result | `CLEARING` / local pending, report bank result but do not claim local completion. |

Retry only local invalidation while worker is offline. Crash with bank `IN_FLIGHT` becomes `UNCONFIRMED` / `LOGOUT_OUTCOME_UNKNOWN`, never a repeated click. Snapshot is removed on bank outcome or five-minute revoke-window expiry; expiry before a bank attempt yields `REVOKE_WINDOW_EXPIRED`. No saved session does not prove bank expiry. Terminal notices use the one delivery loop outside bank lock.

## AI contract and finite diagnostics

NineRouter uses the configured exact vision model with strict crop PNG limits, no redirects and `POST /v1/chat/completions` multimodal data URI. HTTPS is required except explicitly controlled private Docker HTTP endpoints; userinfo/query/fragment are rejected. Accepted output is supported strict JSON `{"text":"…"}`, 1–16 ASCII alphanumeric, never a permissive Markdown/prose parser.

Preflight discovery `/v1/models/image-to-text` falls back to `/v1/models` for 404/405 or unsupported schema/list without the exact model, not auth/rate-limit/timeout. Exact non-combo discovery must be followed by synthetic `AB12CD` OCR. Overall AI check deadline 45 seconds; discovery <=10 seconds/request, OCR <=25 seconds; runtime solve <=25 seconds with context/attempt limits. No blind retry, repeated tick preflight or automatic model fallback.

`DiagnosticError` unwraps to unavailable and retains only safe stage/code/integer HTTP status. Stages: `MODEL_DISCOVERY`, `SYNTHETIC_OCR`, `CAPTCHA_OCR`. Finite codes: `DNS`, `TLS`, `TIMEOUT`, `CANCELLED`, `NETWORK`, `HTTP_AUTH`, `HTTP_NOT_FOUND`, `HTTP_RATE_LIMIT`, `HTTP_UPSTREAM`, `MODEL_NOT_FOUND`, `MODEL_COMBO_UNSUPPORTED`, `RESPONSE_TOO_LARGE`, `RESPONSE_SCHEMA`, `REFUSAL`, `OCR_MISMATCH`. No raw endpoint, key, provider body or output is retained/logged. The runbook maps every code to operator action.

`--check-ai` loads AI configuration and runs synthetic vision without Telegram/DB/import/bank access. `--check-config` adds bot config/private-destination checks but never polls/registers commands/logs in. Neither validates ACB password. `setup-recovery.sh --configure-ai` stages candidate key/env, synthetic-preflights the candidate mount, then admission-promotes and force-recreates even key-only changes. On activation failure, restore both prior key/env and recreate prior admitted runtime; concurrent modifications are not overwritten. AI failure offers explicit repair, AI-off/human fallback or stop (default), never silent degradation. Provider privacy/retention needs owner validation before any bank crop leaves the controller.

## Verification, catch-up and data continuity

`Finalizer.Complete` is idempotent. Worker verification failure cannot overwrite the session/checkpoint or report success. Once verified commit exists, restart resumes durable intent rather than exporting a second handoff or requesting another OTP. Session encryption remains generation-bound; logout/save fences make stale finalizers unable to commit.

Atomic session commit sets `MONITORING`, durable recovery run/required range and episode `CATCHING_UP`. The linked recovery gate blocks realtime/payment boost, keepalive, sync admission and queued history before bank I/O, allowing verification/catch-up. Controller/Telegram outage and failed/cancelled catch-up do not release it; unrelated QR/config/history behavior is retained.

Required coverage starts at the earliest valid checkpoint coverage end, episode local date or today minus six days (including an already frozen required start), through today Asia/Ho_Chi_Minh. Long outages are scanned per day with existing pagination/retries/semantic dedup, not clipped to seven days. Existing startup/onboarding seven-day semantics remain. Corrupt/future checkpoint is `INVALID_CHECKPOINT`, requiring controlled correction, not reset; unavailable required bank range is `HISTORY_RANGE_UNAVAILABLE`, not false completion.

Worker completion validates durable coverage and creates immutable gap-tail extension runs if midnight or stopped-worker time extended the range. Only full coverage closes the episode/releases the gate and queues readiness independently of Telegram. Non-auth retry preserves run/session/range/progress. Auth loss during catch-up invalidates old consent/challenges and requires a fresh login button.

## Deployment and evidence boundaries

Controller is an optional `auth-recovery` profile using the worker image, non-root/read-only, shared SQLite/WAL, private networks, no host port/Docker socket. Runtime and importer use the same `.auth-recovery.lock`; no second polling consumer is allowed. `storage.OpenRuntime` never migrates. Existing dbtool/pipeline admission applies schema 13/checksum, safely invalidates unverified legacy attempts and retains healthy monitoring/committed catch-up.

Deployment admission precedes replacement/reconfiguration; active login blocks it rather than canceling an OTP. Import runs after admission release and before controller start, not by bypassing a locked mutation gate. Missing import files with absent credentials leave degraded menu. Readiness is local initialization, not external credential acceptance. Disable recovery through admitted flag-off and fix forward if needed; do not restart legacy auto-login code or restore an old DB to roll back this cutover. Preserve master/internal tokens, transaction history and healthy bank sessions.

Current changed Go boundary suites and final scoped native Windows races passed; an earlier full `go test -count=1 ./...` passed, but the latest aggregate run failed unchanged `internal/realtimestream` `TestClientHeartbeatPreventsIdleTimeout` (stream idle timeout instead of caller deadline) and `internal/ttsclient` `TestClientSynthesizeStream` (zero FirstByteDuration). All affected/new packages passed. No final clean full-suite claim is made and these failures were not rerun to hide them. Actual Chromium login/revocation/health fixtures, credential desktop/mobile browser save, finite-diagnostic process fixture, frontend suite/build and route-render smoke are recorded with their exact scope in the runbook. The final Chromium run passed tightened logout-form guards, rejecting unrelated background POST and permitting the exact native logout form once. These do **not** establish live ACB DOM compatibility, provider privacy/real model acceptance, Telegram operator UX or actual production edge/deploy behavior.

Linux Python/Docker deployment permission/admission/remount/rollback acceptance, final production HTTPS CSP/Access/Cloudflare injection settings, owner-operated private bot and real provider/bank flows still require access unavailable in this execution. No production secrets were inspected or healthy session disrupted. Previous 2026-10-02 harness outcomes describe the old implementation and are not relabeled as current end-to-end consent/menu/logout acceptance.
