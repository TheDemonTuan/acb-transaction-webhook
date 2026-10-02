# ACB Session Recovery Design

> Updated 2026-10-02. This document supersedes the earlier human-only CAPTCHA / AI page-classifier design and the generation-scoped retry budget. The companion [operations and acceptance runbook](2026-09-15-acb-session-recovery.md) describes rollout. Production recovery remains disabled until the live DOM, credentials, private Telegram bot and controlled maintenance-window gates are satisfied; fixture results are not live ACB acceptance.

## Goal and boundaries

Recover an established ACB connection after durable `AUTH_REQUIRED` without requiring VPS/noVNC interaction in the supported normal flow: stored credentials → **9router CAPTCHA OCR first when enabled, private Telegram human fallback** → **human login OTP always** → worker session verification → encrypted persistence → worker-owned day-by-day catch-up → release automatic recovery gate → recovery notice. Existing admin/manual auth and noVNC remain the escape hatch.

- The controller does not perform bank HTTP verification or history scans, transfer money, generate/resend OTP, or bypass SafeKey/push/QR/biometric authentication.
- AI is not a page classifier, browser agent or fallback for unknown DOM. It receives only a safe CAPTCHA crop, never credentials, account data, balances, transactions, OTP, cookies, DOM, browser storage or handoff.
- Only confirmed upstream auth failure changes polling state; a login-like/CAPTCHA/OTP page that is inconclusive remains `AUTH_INCONCLUSIVE` rather than being treated as confirmed expiry.
- Local session loss is also durable: `SESSION_MISSING` and `SESSION_INVALID` request recovery; `SESSION_DECRYPT_FAILED` requires manual intervention rather than masking a master-key problem. Unclassified DB/restorer/network errors do not change auth generation. Checkpoints, journal and the old envelope are preserved.
- The watcher auto-starts only a connection with prior operational evidence (verified session/attempt, successful poll or completed recovery). A missing session row does not erase that evidence. Initial onboarding requires existing account configuration plus an operator-confirmed `/acb_login`, or manual auth.

## Component ownership and topology

```text
worker: confirmed auth loss / local restore loss
  -> SQLite AUTH_REQUIRED + durable episode
  -> recovery-controller (singleton, durable reconciliation)
  -> auth-browser (actual Chromium, same session across login and OTP)
       CAPTCHA crop -> 9router once -> answer or Telegram fallback
       OTP prompt  -> exact private Telegram operator
  -> shared authsession.Finalizer
       encrypted envelope -> worker RPC VerifySession
       atomic session + recovery intent + automatic gate commit
  -> worker scheduler: catch-up each required day
  -> worker transaction: midnight extension or COMPLETED + gate release
  -> controller: deliver recovery notice
```

| Component | Contract and responsibilities |
| --- | --- |
| `cmd/recovery-controller` / `internal/authrecovery` | Independent process using `storage.OpenRuntime`, not migrations or monitor/catch-up. `RunSingleton` holds `DATABASE_PATH + ".auth-recovery.lock"` before polling Telegram or starting a browser; another instance exits without consuming updates. Reconciles durable state every 5 seconds, observing active browser progress approximately every 1.5 seconds. Automatic DB owner is literal `system:acb-recovery`. |
| `cmd/auth-browser/main.go` + `automation.go` | Production CDP/Chromium implementation, internal token middleware, session TTL and existing noVNC/manual functionality. The unused wrapper is not the automation control plane. |
| `internal/challenge.Broker` | Durable prompt correlation, format/expiry/revision checks, atomic one-shot consume before browser action. Replies exist only in memory. |
| `internal/telegramauth` | Bot API HTTPS long polling; configured private chat and exact user, Vietnamese plain-text UX, confirmations, notices and durable update offset. |
| `internal/captchasolver.NineRouter` | `Solve(ctx, png) (string, error)`; one crop-only OCR request per attempt. Provider failure degrades AI and falls back to human input. |
| `internal/authsession.Finalizer` | Shared manual/automatic handoff → generation-bound AAD encryption → worker `VerifySession` → `CompleteAuthSession` → best-effort browser completion / `ScheduleRecovery`. RPC receives encrypted session material, not the login password. |
| Worker / `internal/monitor` | Sole owner of bank HTTP verifier, catch-up scheduler, transaction deduplication, checkpoints and gate release. Controller/Telegram failure never terminates the worker or gateway. |

No DB transaction is held across browser, Telegram, AI or worker RPC I/O. The controller rechecks current connection, generation, configuration revision, attempt and expiry before side effects; browser revisions fence stale submissions. This prevents stale commits, but does not pretend cross-process I/O is an atomic DB transaction.

## Durable metadata and fencing

Migration `012_auth_recovery.sql` (`AuthRecoverySQL`, schema v12) adds `auth_recovery_episodes`, `auth_challenges`, `telegram_auth_state`, `telegram_auth_actions` and `auth_recovery_notices`. Timestamps follow RFC3339Nano UTC; Telegram IDs are signed 64-bit integers. These tables hold IDs, counters, state, expiry and message correlation only: no raw Telegram update JSON, challenge response, image/base64, password, OTP hash or handoff. Session material continues to use the existing encrypted session table.

- At most **one open episode and one active auth attempt per connection**, not merely per generation. Manual and automatic attempts share transactional admission and the deployment mutation gate.
- An episode has immutable trigger identity `(connection_id, trigger_generation)`, current generation/config revision, linked attempt/run, monotonically increasing `attempt_count`, `budget_start_count`, persisted retry/cooldown times and frozen required coverage range.
- `EnsureAuthRecoveryEpisode`, `StartRecoveryAuthAttempt`, `FinishRecoveryAuthAttempt` and `TransitionAuthRecovery` fence durable state. Start/finish generation changes, automatic attempt expiry and catch-up auth loss update the same episode and invalidate old challenges **without resetting its budget**.
- External config/generation change supersedes the old episode and invalidates old actions/prompts. Cancellation targets only its old attempt, never a newer/manual browser. Existing manual attempts are waited for, not stolen or canceled.
- `CANCELLED` records the post-finish generation and suppresses automatic recreation. An operator confirmation rearms the same episode; it does not insert a duplicate trigger identity.

Episode states (separate from the unchanged `auth_attempts` status vocabulary):

```text
DETECTED -> STARTING -> LOGIN -> WAITING_CAPTCHA / WAITING_OTP
  -> VERIFYING -> CATCHING_UP -> COMPLETED

Failure/wait: RETRY_WAIT, WAIT_OPERATOR, MAINTENANCE_WAIT, MANUAL_REQUIRED
Terminal: COMPLETED, SUPERSEDED, CANCELLED
```

`MANUAL_REQUIRED` remains open, so reconciliation cannot evade the circuit by creating another episode. `CATCHING_UP` is an episode state, not a new connection enum: the connection becomes `MONITORING` when the verified session commits, while the automatic recovery gate still suppresses polling.

## Fixed budgets and failure policy

Limits are constants, not environment knobs:

| Limit | Scope |
| --- | --- |
| 3 browser attempts | Per connection's outage episode budget: `attempt_count - budget_start_count`; restart and internal generation bumps do not reset it. |
| 3 CAPTCHA submissions | Per browser attempt, **including an AI answer**. Rejection uses a fresh revision and human fallback; exhaustion goes to `WAIT_OPERATOR`, not an automatic new attempt to evade the cap. |
| 1 OTP submission | Per browser attempt. Incorrect, expired, unanswered or unknown-outcome OTP goes to `WAIT_OPERATOR`; no resend/replay/login loop. |
| 1 AI request / at most 1 AI answer submission | Per browser attempt. `ai_used` is committed before calling the provider. |

Operator-confirmed retry/rearm sets `budget_start_count = attempt_count`, preserving the ordinal and notice identity. A minimum **60 seconds since the last login submission** survives restart. Browser attempt TTL is 15 minutes. CAPTCHA TTL defaults to 180 seconds (30–180 configurable), OTP to 120 seconds (30–120); both are bounded by browser and session expiry.

- Browser/network failure: retry after 30 seconds, then 120 seconds, with 0–20% jitter and the same episode cap.
- Maintenance: wait 15 minutes, still within the same cap.
- Proven credential rejection/account lock: `MANUAL_REQUIRED`; ambiguous rejection stays unknown rather than guessing a password or CAPTCHA failure.
- Unknown page: at most 10 observations / 30 seconds, then manual. Unsupported challenge, unsafe crop or ambiguous/account-selection controls fail closed.
- Bot unavailable: no new login submission without a usable operator channel. Pending challenges expire normally; Telegram network retries do not replenish or consume login budget.
- Worker verification unavailable: remain `VERIFYING`, re-observe/retry approximately every 5 seconds within session TTL; never request fresh OTP merely because RPC is unavailable.
- DB unavailable: no new side effect using cached generation; reconcile later.

## Browser automation contract

`internal/authbrowser.Client` exposes `Observe`, `CaptureCaptcha`, `SubmitLogin`, `SubmitCaptcha`, and `SubmitOTP`. Internal routes are `GET /sessions/{attemptID}/observation`, `GET /sessions/{attemptID}/captcha?revision=...` and `POST /sessions/{attemptID}/{login,captcha,otp}`. JSON action bodies are limited to 8 KiB; CAPTCHA PNG to 512 KiB. Observation/action/image/handoff responses use `Cache-Control: no-store`; responses never echo answers.

An opaque revision describes a live form/challenge. It changes on navigation, form/image replacement, new validation or begun submission; ordinary observation does not rotate it. Browser actions serialize with observer/handoff work and consume the revision **before clicking**, so timeout is never a reason to replay a click or OTP. Wrong attempt, revision/state conflict, expiry and unsupported controls fail closed.

Visible, enabled, uniquely matching controls must share a checked same-origin form. No arbitrary controller-provided selector/JS and no wrong-origin iframe access. An initial CAPTCHA is captured **before** credentials are submitted: `SubmitLogin` fills credentials and CAPTCHA once. A separate later CAPTCHA uses `SubmitCaptcha`. Only proven authenticated signals plus a valid history form with the **exact `AccountNbr` from `ACB_ACCOUNT_FILE`** allow handoff; a masked/first account is insufficient.

CAPTCHA capture is the proven image/canvas node or its safe bounding rectangle only. There is **no full-page screenshot fallback**. Current structural selector fixtures are not evidence of current live ACB login/CAPTCHA/OTP compatibility. Before enabling production, use the account owner's existing manual/noVNC session to validate sanitized control type/name/id, labels, form action and error codes; do not save secret values, raw HTML, cookies or tokens. Unsupported SafeKey/push/QR/biometric/device binding requires the bank app/manual flow, not AI guessing.

## Telegram authorization, replay and privacy

Use a dedicated BotFather bot with one private operator chat, exact nonzero `TELEGRAM_CHAT_ID` / `TELEGRAM_USER_ID`, and no second `getUpdates` consumer. `/start` provides help, not enrollment or authorization. `getMe` / `getWebhookInfo` preflight refuses a configured webhook with `TELEGRAM_WEBHOOK_CONFLICT`; it never deletes a webhook automatically.

Long polling uses 30-second timeout / 40-second HTTP deadline, limit 100 and only `message` / `callback_query`. Persist `next_update_id` only after durable disposition, including rejected updates. 429 respects clamped 1–300-second `retry_after`; transient failures back off to 60 seconds with jitter. 401/403 stops bot transport without terminating bank worker/gateway.

Authorization happens before reading reply/command content: exact private chat, exact non-bot user; reject forwarded, edited, via-bot and media/document/contact messages. Callback data is only `ar:<random-action-id>`, bound to the persisted originating message, user/chat, generation and 60-second TTL. One-shot command effects and action consumption commit together; authorized stale callbacks are answered to stop the spinner.

Challenge delivery is `DELIVERING -> PENDING` only after the returned prompt message ID commits. Direct replies derive the challenge from that message ID, not an ID typed/quoted by the operator. Atomic `PENDING -> CONSUMING` checks generation, attempt, revision, chat, prompt, TTL and current state before submission. Trim edge whitespace, preserve leading zero OTP and CAPTCHA case. OTP is ASCII digits 4–10, CAPTCHA ASCII alphanumeric 1–16, further bounded by the browser form. Unknown action outcome invalidates the challenge; it is never reset to `PENDING` or replayed.

Prompts use in-memory multipart CAPTCHA photo / login-OTP text, `ForceReply` and `protect_content=true`, local Asia/Ho_Chi_Minh expiry, no parse-mode injection, and short IDs rather than username/account/balance. Deleting accepted answers and prompts is **best effort**. Telegram bot chats are **not end-to-end encrypted**; server and notification history may retain content. Dropping owned references/clearing buffers does not guarantee zeroization of Go strings, JSON or Chromium memory.

Lifecycle notices are transactionally enqueued with keys `episodeID:event:attemptNumber`. Logical deduplication is durable, but Telegram sending is **at least once**: a crash between send and recording the message can duplicate a notice. No claim of exactly-once network notification or webhook delivery is made.

## 9router crop-only OCR

`NINEROUTER_BASE_URL` is normalized to `/v1` exactly once; requests use `POST /v1/chat/completions`, the configured exact vision model, Bearer key from file, `stream:false`, fixed transcription instruction and PNG `image_url` data URI. No tools, browser instructions, page classification or model-selected fallback. Production requires HTTPS, or a deliberately controlled private Docker service hostname over HTTP. Userinfo/query/fragment and redirects are rejected.

The solver validates PNG signature/dimensions/512-KiB maximum, uses a 12-second deadline and 16-KiB response limit, and accepts exactly one JSON object `{"text":"..."}` with a valid answer. Empty/refusal/prose/markdown/extra keys, non-2xx, timeout, quota or malformed output immediately use human fallback, with no second AI call that attempt. Bank-rejected AI answers also go to a fresh human challenge, not another AI call.

`--check-config` checks `/v1/models/image-to-text` (404 fallback to `/v1/models`), exact configured model identity and a synthetic `AB12CD` image. This proves discovery/data-URI wire compatibility on that instance, **not real CAPTCHA accuracy**. Before bank crops leave the controller, verify payload logging/retention controls across 9router, proxy and upstream; `ENABLE_REQUEST_LOGS=false` alone is not a privacy guarantee. AI-disabled mode reads no AI key and retains the full human CAPTCHA + OTP path.

## Verification, catch-up and automatic-only gate

`Finalizer.Complete(ctx, attempt)` is idempotent. A failed worker verification does not overwrite session/checkpoint or emit recovery success. Once committed, restart discovers the verified attempt and run event key without exporting another handoff or generating OTP; browser cleanup and scheduling are safely retryable.

`CompleteAuthSession` atomically commits encrypted session, connection `MONITORING`, recovery run, automatic episode `CATCHING_UP`, and immutable required range. `HasBlockingAuthRecovery` fences realtime (including payment boost), keepalive, manual sync admission and queued history work before bank I/O. Catch-up and verifier may run; blocked history jobs requeue rather than fail. Failed/canceled catch-up and controller downtime retain the automatic gate. **Manual auth/payment QR boost behavior is unchanged**; this gate applies only to linked automatic episodes.

Automatic coverage begins at the earliest of today minus six days, valid checkpoint `coverage_to`, and episode creation local date (plus any already frozen required start), through today in Asia/Ho_Chi_Minh. Missing checkpoint uses available evidence; future/corrupt checkpoint produces `INVALID_CHECKPOINT`, manual intervention and a retained gate, not a reset. A ten-day outage is scanned day by day without the generic seven-day clamp, using existing per-day pagination, transient retries and semantic transaction deduplication. Manual/startup/onboarding semantics retain their existing seven-day range. Upstream inability to serve the required history is explicit `HISTORY_RANGE_UNAVAILABLE`, not false success.

Worker-owned `UpdateRecoveryRunProgress` completion checks durable day coverage. If midnight (or several stopped-worker days) extended the gap, the same transaction creates an extension run `attemptID:gap-tail:YYYY-MM-DD` from the day after the old required end through today and keeps the gate. It does not mutate the old immutable run. Only full coverage closes the episode `COMPLETED`, releases the gate and enqueues success, independently of Telegram availability. Retry of non-auth failed catch-up resets the same run/range/progress with the verified session; no new login/OTP. `INVALID_CHECKPOINT` cannot be bypassed by retry.

## Restart, deployment and acceptance

Persisted budget, pause, offset, cooldown, run and prompt metadata survive process restart. A still-valid `PENDING` prompt can remain usable; stale revision/TTL/browser invalidates it. `CONSUMING` after crash has no plaintext to replay: observe only, continue if OTP/authentication already advanced, otherwise wait for operator. SIGTERM stops new work without blindly finishing a live attempt; lock release follows shutdown.

The controller shares the worker image (`WORKER_IMAGE_REF`, `/recovery-controller`) but remains an optional `auth-recovery` Compose-profile singleton, without a host port/Docker socket. `storage.OpenRuntime` never migrates; apply schema through the existing dbtool/deploy pipeline. Deploy acquires mutation admission before stopping controller; active login blocks deploy, rather than force-canceling the operator's OTP. Start controller after worker/browser health and release the gate before allowing new auth work. Rollback to legacy bundles must stop the new controller, not start a binary absent from old images.

Internal localhost `8182/readyz` / `--readiness-check` measures local runtime/DB/schema/singleton/config initialization, not successful ACB login. Telegram/AI degradation is reported separately and must not fail all deployment services. `--check-config` is the separate credential/webhook/model preflight, with no runtime lock, polling, bank browser or bank CAPTCHA. See the runbook for exact flags, secret provisioning, release-context commands and the distinction between deterministic fixture proof and **not-yet-accomplished live acceptance**.
