# ACB Session Control: Operations and Acceptance

> Updated 2026-10-03. The implemented control plane is **private Telegram menu → authenticated one-use login button → supported browser automation / AI CAPTCHA → human login OTP → worker verification → encrypted session → catch-up**. Username/password changes use a separate OWNER-protected HTTPS page. The [design document](2026-09-15-acb-session-recovery-design.md) defines the boundaries. Local implementation evidence is not production, live ACB, live Telegram or live provider acceptance.

## Operating contract

- A lost session creates a durable warning, **not a login**. Operational history, startup, polling, restart, text commands and “Mở khóa đăng nhập” cannot authorize a new browser attempt. Only a valid LOGIN/RETRY button gives one attempt; after failure or another session loss, another button is required.
- `/start`, `/menu` and `/help` show the Vietnamese control panel. Use “Trạng thái”, “Đăng nhập”, “Đổi thông tin đăng nhập”, “Đăng xuất ACB” and “Trợ giúp”; cancellation and catch-up retry appear when applicable. Read-only menu/status remain available during deployment maintenance.
- Without an initialized connection/encrypted credential record, the bot reports **“Chưa khởi tạo — chạy setup/import trên VPS”**. Missing credentials permit a degraded menu, not bank login or a grant for an empty connection.
- After a login button, supported CAPTCHA/login/verification/catch-up steps proceed automatically. Supply only the requested **login OTP from the ACB app** by replying to the current private-chat prompt. Human CAPTCHA is the fallback when AI or a safely recognized challenge cannot complete that step.
- No dashboard login, bank browser iframe, manual login API or remote-desktop escape hatch remains. Unsupported bank challenges require the ACB app or bank support; the bot must stop rather than guess controls.
- Dashboard status, tracking configuration, QR, transaction history and data sync remain. Changing stored credentials or logging out preserves the connection ID, exact tracked account, checkpoints and transaction journal.

## Initial setup and migration import

Deploy the new pipeline-built release and apply schema 13 through deployment admission **before** setup. From the VPS:

```bash
sudo bash /opt/bank-event-gateway/deploy/setup-recovery.sh
```

For a different installation root, append `--deploy-path /actual/root`. The stable wrapper resolves its installed release companion; do not run a copied script against an unrelated image/runtime file.

Initial setup verifies the bundle, existing image/schema, master key and internal tokens; provisions the dedicated Telegram bot and exact numeric private user/chat; and verifies operator access with a synthetic private ForceReply. Open the dedicated bot and send `/start` first. Setup must not delete an existing webhook or compete with a running controller's polling.

Initial username/password/exact account input is hidden. Import-only host files `deploy/secrets/acb_username`, `acb_password` and `acb_account` are mounted readonly at `/run/import/acb_username`, `/run/import/acb_password` and `/run/import/acb_account` **only in a one-shot importer**. The directory is 0700 and regular, non-symlink secret files are 0600, owner UID/GID 1000. Preserve significant password spaces; username is trimmed, account is ASCII digits, and empty/CR/LF/NUL input is rejected according to the importer contract. Preserve the existing master key; a decrypt failure is not a reason to replace it.

The importer writes AES-GCM ciphertext to the authoritative DB only when no credential record exists. It never overwrites a username/password subsequently changed through HTTPS. If a decryptable stored session exists, its exact `AccountNbr` must match the imported account; otherwise import fails `CREDENTIAL_ACCOUNT_MISMATCH`. Without a verified session, import does not prove the account or password correct; exact-account verification occurs during a later button-authorized login.

For an already enabled installation needing legacy-file migration:

```bash
sudo bash /opt/bank-event-gateway/deploy/setup-recovery.sh --import-credentials
```

This path does not enroll/poll Telegram, preflight AI or log in to ACB. It uses deployment admission, stops/restarts the singleton safely, and imports only when DB credentials are absent. An existing record is not overwritten. Do not stop an active OTP attempt to migrate: wait or deliberately cancel it in Telegram first. The normal deploy start helper also performs the admitted one-shot import before controller startup when all three legacy files exist. Missing files with no DB record leave the bot degraded, requiring setup/import; there is no runtime file fallback. Legacy host files remain the owner's files and are not automatically deleted.

Setup validates an isolated candidate before activation. If AI preflight fails, choose explicitly: **(1)** correct URL/model/key and retry synthetic vision, **(2)** activate with AI off and human CAPTCHA fallback, or **(3)** stop, the default. AI is never silently disabled. Re-running ordinary setup when recovery is already enabled checks readiness; it is not a password-update mechanism.

Setup success means configuration, the exercised private reply and local readiness, **not successful bank authentication**. Enabling the controller does not initiate login and does not force logout of a healthy monitoring session.

## Operational username/password changes

1. Open `/menu` → **“Đổi thông tin đăng nhập”** → confirm **“Cấp link đổi thông tin”**. Confirmation is a one-use 60-second action. The new link expires after five minutes and revokes an older pending link.
2. Open the HTTPS link in a browser with Cloudflare Access **OWNER** authentication. A Telegram in-app browser without the Access session may need the normal browser. There is no Access bypass.
3. The page `/admin/acb-credentials#grant=…` removes the fragment from history after reading it into RAM. Do not forward the link; possession is one layer, OWNER authentication is another. The page does not put the grant/password in query parameters, local/session storage, analytics or a query cache.
4. Enter the complete username, password and identical confirmation; the old secret is not prefilled. Password leading/trailing spaces are significant. The displayed account is masked and **cannot be changed**. This updates only the system's stored credentials, not the bank password.
5. If `ACB_SESSION_BUSY`, use Telegram logout for a healthy session, or wait/cancel an active login; an active catch-up/logout also blocks save. Opening/validating the link neither disconnects the bank nor consumes the save grant.
6. On successful save, credential revision increases, obsolete actions/challenges/sessions are fenced, and the form clears secrets. The bot says **“Đã lưu thông tin đăng nhập. Chưa đăng nhập ACB.”** Return to Telegram and deliberately press “Đăng nhập”. Save never tests the password or automatically logs in.

A single successful save consumes the grant. `CREDENTIAL_GRANT_EXPIRED` (410) requires a fresh Telegram link; `CREDENTIALS_REVISION_CONFLICT` (409) means state/revision changed; malformed input is `INVALID_CREDENTIAL_INPUT` (400). `CREDENTIALS_UNAVAILABLE` (503) rolls back the write rather than losing the prior password. If HTTP times out after submission, **do not automatically resubmit**: inspect Telegram status and request a new link if needed. Replay cannot increment the revision again.

The private credential APIs require OWNER, CSRF and exact configured `PUBLIC_ORIGIN`; validation is limited to 10 requests/minute per authenticated owner. Neither API returns plaintext credentials, full account, token hash or bank session material. The public viewer host must continue to deny `/admin` and private APIs.

## Login, challenges and logout

### Login controls

| Control | Effect |
| --- | --- |
| `/start`, `/menu`, `/help` | Render menu/help; no enrollment or bank side effect. |
| `/acb_status` / “Trạng thái” | Read durable status; no bank request or finalization. |
| `/acb_login` | Render a fresh LOGIN button; typing the command is not consent. |
| “Đăng nhập” | Consume the authenticated, message-bound, revision-bound 60-second action for **one** browser attempt. A stale button only yields a new button. |
| `/acb_retry` / retry button | New login consent when recovery needs it; if a verified session only needs catch-up retry, reuse that run/session with no new login or OTP. |
| `/acb_pause` / “Khóa đăng nhập” | Lock recovery login; does not log out a verified monitoring session or erase history. |
| `/acb_resume` / “Mở khóa đăng nhập” | Unlock availability, **not login consent**. |
| `/acb_cancel` / “Hủy đăng nhập” | Confirm cancellation of an uncommitted login, not bank logout of a healthy session. |

Fixed bounds: one browser attempt per consumed consent, consent admission within 60 seconds, attempt TTL 15 minutes, login submission cooldown 60 seconds, at most **3 CAPTCHA submissions, 3 AI requests for distinct CAPTCHA revisions, and 1 OTP submission per attempt**. Restart does not replenish counters or replay a consumed revision. Error, maintenance, expiry or uncertain outcome ends authority to start another attempt; a retry time is only when a new button is permitted, never a timer-driven login.

Only directly reply to the current CAPTCHA/OTP prompt. Exact private chat/user, prompt message, attempt, generation, browser revision and TTL must match. OTP is 4–10 ASCII digits, preserving leading zeroes, with at most 120 seconds or the shorter browser expiry. Do not submit transfer/payment OTP, challenge IDs, forwarded or edited messages. CAPTCHA input is 1–16 ASCII letters/digits subject to the actual form.

Replies are durably consumed before bank submission. A crash/timeout after `CONSUMING` cannot replay the plaintext answer. A DB failure before durable disposition must not silently delete input and advance its offset. Still-valid pending prompts may be revalidated on restart; stale/undelivered prompts are invalidated. A new captcha image by itself is not proof that login was rejected: credentials may be submitted again only after explicit CAPTCHA rejection and a fresh revision, within the same budget. Unknown login outcome requires operator intervention.

Progress edits one message per episode, coalescing observed states: opening ACB → CAPTCHA → login → waiting OTP → verifying → catch-up → ready. Prompt/final/session-loss notices are separate. No fake percent or bank response-time guarantee. Notices are at least once: a crash can duplicate a notification, **not authorize a duplicate bank action**.

“Xem ảnh captcha” uses only the current safely bounded crop, at most 512 KiB, with protected content and best-effort deletion. No full-page/password/OTP/account/balance/history screenshot fallback is permitted. If safe crop cannot be established, stop with `UNSAFE_CAPTCHA_CROP`.

### Logout reports two independent outcomes

Use `/menu` → **“Đăng xuất ACB”** → its 60-second confirmation. Navigation does not log out. The confirmed action durably fences the old generation, pauses login, invalidates actions/prompts/grants, cancels obsolete work and removes the persisted session while retaining credentials/checkpoints/journal. Worker invalidation then clears **both monitoring and verifier memory**, including cookie jars and continuation tokens; old queued or paginated requests cannot resurrect the session.

The controller tries one bank revocation through supported official ACB DOM controls, not an invented logout HTTP operation or login-to-logout flow. Confirmation requires a protected read-only probe with the **pre-logout authentication cookies** to demonstrate that the previously authenticated session is now rejected. An empty post-click cookie jar or successful `DeleteSession` is not bank-logout proof.

| Report | Meaning / action |
| --- | --- |
| `COMPLETED`, bank `CONFIRMED` | Worker local clear acknowledged and bank rejection of the former session demonstrated. |
| `COMPLETED`, bank `ALREADY_EXPIRED` | Worker local clear acknowledged; a pre-action protected probe proved that bank session was already expired. |
| `LOCAL_ONLY`, bank `UNCONFIRMED` | Local clear acknowledged, but bank revocation was not established: unsupported/ambiguous control, unavailable snapshot, network/unknown outcome, or other safe reason. Use the ACB app/bank support if bank-side revocation is required. |
| `CLEARING` / local pending | Bank outcome is reported separately, but worker memory-clear acknowledgement is still pending. LOGIN and credential save stay blocked; do not call local logout complete. |

Worker invalidation can retry safely while offline. Bank click cannot be blindly retried after timeout/restart; an in-flight crash becomes `LOGOUT_OUTCOME_UNKNOWN`. Encrypted revoke snapshots are removed after a bank outcome or the five-minute revoke window. Having no saved session means **no bank-revocation evidence**, not `ALREADY_EXPIRED`. “Mở khóa đăng nhập” afterward still requires a new LOGIN button.

## AI preflight, privacy and diagnosis

AI transcribes only a CAPTCHA crop. Before bank crops leave the controller, verify router/proxy/upstream logging and retention with the owner. `ENABLE_REQUEST_LOGS=false` covers one layer, not all layers. Never send full screenshots, DOM, cookies, username/password, OTP, balances or transaction history to AI. Telegram bot chat is **not E2EE**; `protect_content` and deletion cannot prevent screenshots or erase all server/notification copies. VPS/master-key compromise can expose credentials; this is not absolute security.

Use the existing configuration without keys in command arguments:

```bash
docker exec acb-recovery-controller /recovery-controller --check-ai
```

This runs **synthetic vision only**, not Telegram polling, credential import, DB access or ACB password validation. Run it when AI is actually configured/enabled; an AI-disabled invocation is not model acceptance. To correct URL/model/key or rotate only the AI key:

```bash
sudo bash /opt/bank-event-gateway/deploy/setup-recovery.sh --configure-ai
```

Use the same `--deploy-path /actual/root` option if required. New keys are hidden input, staged in an isolated candidate file and preflighted before promotion. Admission rechecks concurrent changes; promotion of a key, even key-only rotation, **force-recreates** the controller so its bind mount sees the new inode. Activation failure restores both prior environment and key and recreates the admitted prior runtime; unresolved rollback is explicit. This is configuration rollback within the new architecture, not permission to restart a legacy auto-login binary. Linux Docker key-only rotation/rollback still requires deployment acceptance below.

Model discovery first uses `/v1/models/image-to-text`, then `/v1/models` only for an unsupported endpoint (404/405) or schema/list without the exact model. It does not add fallback calls after auth failure, rate limit or timeout. An exact non-combo model must then transcribe synthetic **`AB12CD`**. Discovery alone does not prove vision. Output must be exactly the supported JSON `{"text":"…"}` with 1–16 ASCII alphanumeric characters; no Markdown/prose salvage or auto-selected model.

AI check deadline is 45 seconds overall, discovery at most 10 seconds/request, OCR at most 25 seconds; runtime OCR also respects attempt/context expiry. No blind retry. A revision is claimed durably before I/O, with at most three unique-revision calls per attempt. AI outage/budget exhaustion gives human CAPTCHA fallback, not automatic credential/OTP replay.

Safe diagnostics contain only `reason=AI_VISION_PREFLIGHT_UNAVAILABLE`, stage `MODEL_DISCOVERY` / `SYNTHETIC_OCR` (runtime `CAPTCHA_OCR`), a finite code and optional integer HTTP status. Example: `stage=MODEL_DISCOVERY code=HTTP_AUTH http_status=401`. No provider body, raw transport error, key, URL or model output belongs in application logs or Telegram.

| Code | Operator action |
| --- | --- |
| `DNS`, `NETWORK` | Check controller-network DNS/egress and configured endpoint reachability. |
| `TLS` | Check hostname, trust chain and certificates; **do not disable TLS verification**. |
| `TIMEOUT`, `CANCELLED` | Check request/context deadlines and service availability; do not add blind retries. |
| `HTTP_AUTH` | Correct API secret/entitlement (401/403); unrelated to ACB password. |
| `HTTP_NOT_FOUND` | Correct base URL and `/v1` API path (404/405). |
| `HTTP_RATE_LIMIT` | Check upstream quota/retry-after before a deliberate recheck. |
| `HTTP_UPSTREAM` | Check provider availability/status at the provider; do not relay raw responses. |
| `MODEL_NOT_FOUND` | Configure an exact model ID actually offered by the provider. |
| `MODEL_COMBO_UNSUPPORTED` | Select an exact vision model, not a combo. |
| `RESPONSE_TOO_LARGE` | Check provider response contract/size; retain the response cap. |
| `RESPONSE_SCHEMA` | Check multimodal/strict JSON response compatibility; no permissive parser workaround. |
| `REFUSAL` | Investigate model/provider policy at the provider; human fallback until accepted. |
| `OCR_MISMATCH` | Synthetic text was not exactly `AB12CD`; do not claim preflight pass. |

If the bank password was saved incorrectly, use import if still uninitialized, then the Telegram HTTPS credential link. AI preflight cannot diagnose or repair that password. Explicitly selected AI-off mode can make the bot/human fallback available while the provider remains unaccepted; it is not a provider fix.

## Configuration, deployment and readiness

Use strict `true`/`false` flags, default `AUTH_RECOVERY_ENABLED=false`, `AI_CAPTCHA_ENABLED=false`. `.env.example` describes non-secret configuration. Protected production environment is `deploy/.env.production`; release digest/runtime files remain immutable. Never put bank passwords, bot/router keys, master key or cookies in Git, image layers, `.env`, chat, logs or argv.

| Runtime field | Contract |
| --- | --- |
| `DATABASE_PATH` | Persistent shared SQLite/WAL, normally `/data/gateway.db`. |
| `PUBLIC_ORIGIN` | Canonical HTTPS dashboard origin; used for link generation and exact Origin validation, never request Host. |
| `AUTH_BROWSER_URL`, `WORKER_RPC_URL` | Private `http://auth-browser:8181`, `http://worker:8190`; no public browser/CDP/RPC route. |
| `APP_MASTER_KEY_FILE` | Existing `/run/secrets/app_master_key`, shared for encrypted DB material. |
| `AUTH_BROWSER_INTERNAL_TOKEN_FILE`, `WORKER_INTERNAL_TOKEN_FILE` | Controller's private browser/worker tokens; gateway has no browser token/control client. |
| `TELEGRAM_BOT_TOKEN_FILE` | `/run/secrets/telegram_bot_token`; dedicated single-consumer bot. |
| `TELEGRAM_CHAT_ID`, `TELEGRAM_USER_ID` | Exact numeric configured private owner, not username/group allowlists. |
| `AUTH_RECOVERY_CAPTCHA_TTL_SECONDS`, `AUTH_RECOVERY_OTP_TTL_SECONDS` | Defaults 180 and 120, permitted 30–180 / 30–120, bounded by browser expiry. |
| `NINEROUTER_BASE_URL`, `NINEROUTER_CAPTCHA_MODEL`, `NINEROUTER_API_KEY_FILE` | Required for enabled AI only; HTTPS or explicitly controlled private Docker HTTP host, exact non-combo vision model, `/run/secrets/ninerouter_api_key`. |

Runtime has no `ACB_*_FILE` credential inputs or VNC configuration. The three initial files are import-only; subsequent username/password operations use the encrypted DB and HTTPS grant. AI key mount is conditional through `deploy/compose.auth-recovery-ai.yaml`; non-AI mode does not require/read it.

- Controller uses `storage.OpenRuntime`; it **never migrates on startup**. Apply `013_telegram_session_control.sql`, schema 13/checksum **`2026-10-02-v13-telegram-session-control`**, through existing dbtool/pipeline admission. Do not modify deployed migration 012/checksum.
- Migration invalidates old callbacks/challenges and cancels legacy unfinished, unverified attempts without deriving consent from old state. Preserve healthy `MONITORING` sessions and committed verification/catch-up. Do not force logout to prove the feature.
- Optional `auth-recovery` profile uses `WORKER_IMAGE_REF`, `/recovery-controller`, shared DB volume, private core/egress networks, non-root/read-only/capability restrictions, no host port or Docker socket. Chromium/display remain private; VNC packages/ports are removed.
- Runtime and importer acquire `DATABASE_PATH + ".auth-recovery.lock"`. There is one long-poll consumer and one delivery loop. `--check-config`/`--check-ai` do not poll or acquire the runtime singleton lock.
- Acquire mutation admission before controller/worker/browser replacement. Active auth blocks deployment; wait or deliberately cancel through Telegram, never force-cancel OTP. Release the mutation gate before importer/runtime mutations; start only after release/worker/browser health prerequisites.
- On failure after this cutover, disable recovery through **deployment admission** (`AUTH_RECOVERY_ENABLED=false`) while retaining worker/DB, then fix forward. Do not run an old binary that auto-starts login, alter checksums, restore an old DB blindly or lose transaction history to roll back.
- Local readiness (`127.0.0.1:8182/readyz`, `/recovery-controller --readiness-check`) measures initialized local runtime/DB/schema/config, not correct bank credentials or live acceptance. Telegram/AI degradation is reported separately.

Run release-aware preflight only in the installed release context. After sourcing that release's `deploy/simple-lib.sh`, with `DEPLOY_PATH`, `release` and `runtime` set to the installation's real paths:

```bash
compose_release "$release" "$runtime" config --quiet
compose_release "$release" "$runtime" run --rm --no-deps recovery-controller --check-config
```

The helper selects the enabled profile/AI override. Do not invent digest files or enable flags just to make an unrelated checkout pass. `--check-config` calls Telegram `getMe`/`getWebhookInfo`/configured private chat plus enabled synthetic AI; no `getUpdates`, `setMyCommands`, bank browser, import or bank login. A conflicting webhook is refused, not deleted. Runtime sets private-chat command discovery once, then long polls.

### Credential-page edge requirements

The generated production route must use exact `/admin/acb-credentials`, priority 250, the active blue/green frontend service, `tunnel-only` and **`acb-credentials-security`**, not the general security middleware that would overwrite this policy. Nginx exact SPA location retains headers without an internal redirect. Final browser response must have `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, and:

```text
default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; object-src 'none'; script-src 'self'; script-src-attr 'none'; connect-src 'self'; img-src 'self' data:; font-src 'self'; style-src 'self' 'unsafe-inline'
```

Cloudflare Access OWNER configuration and disabling Rocket Loader/analytics/script injection for this exact path are **owner deployment prerequisites**. A correct rendered YAML or Vite response is not evidence of final Cloudflare/Traefik/nginx headers. Verify the actual HTTPS browser response and public-viewer denial before accepting credential operations in production.

## Verified recovery and retained data boundaries

Browser handoff must identify the exact immutable imported account. Worker verification precedes atomic encrypted session commit, `MONITORING`, durable catch-up intent and the recovery gate. `MONITORING` alone is not evidence that realtime resumed.

The recovery gate blocks realtime/payment boost, keepalive, sync admission and queued history bank I/O while allowing verification and catch-up. Failed/cancelled catch-up or controller downtime retains the gate. Required recovery coverage begins at the earliest valid checkpoint coverage end, outage episode date or today minus six days, through today in Asia/Ho_Chi_Minh; long outages are scanned day by day, not silently clipped to seven days. Existing startup/onboarding range semantics remain. Pagination, transaction deduplication, QR and journal/checkpoints are retained, not replaced by the Telegram control plane.

Worker completion checks durable coverage; midnight/stopped-worker gaps add an immutable extension run and retain the gate until covered. Only full completion announces readiness/realtime according to the configured monitoring schedule. Non-auth catch-up retry reuses session/run/range/progress, not login/OTP. `INVALID_CHECKPOINT` needs controlled data correction; `HISTORY_RANGE_UNAVAILABLE` retains the gate. Session loss during catch-up invalidates prior login authority/challenges and waits for a new button.

## Observed evidence and remaining acceptance

**Current execution evidence (2026-10-03), not the previous 2026-10-02 harness:**

- Changed Go boundary suites passed. An earlier integrated `go test -count=1 ./...` passed, but the **latest aggregate run was not clean**: unchanged `internal/realtimestream` `TestClientHeartbeatPreventsIdleTimeout` expected caller deadline but got stream idle timeout, and unchanged `internal/ttsclient` `TestClientSynthesizeStream` got zero `FirstByteDuration` instead of a positive value. All affected/new packages passed in that run. These timing failures were not rerun or hidden; no final full-suite PASS is claimed and no temporary package counts are pinned.
- Final native Windows CGO/GCC `go test -race -count=1 ./internal/storage ./internal/telegramauth ./internal/challenge ./internal/authrecovery ./internal/monitor ./internal/workerrpc ./cmd/worker` passed for all seven scoped packages after review fixes. This is not a full-repository race claim. Scoped storage/recovery regressions also passed for legacy session AAD logout and three failed clicks followed by a fourth fresh consent.
- Final actual Edge Chromium run passed `TestBrowserAutomationInitialCaptchaAndOTP`, `TestBrowserSessionRevocation`, `TestBrowserHealthRealChromium` and `TestRevokeFormPermitRejectsUnrelatedAndDuplicateRequests`. Tightened guards rejected an unrelated background POST with zero server arrivals and permitted the exact native logout form once. Actual browser about:blank/CDP health passed after VNC removal. These are browser/HTTP fixtures, not live ACB logout evidence.
- Actual gateway + Vite sandbox browser on desktop/mobile showed masked account, fragment/history cleanup and no grant in browser storage. Save preserved leading/trailing password spaces encrypted at revision 2, kept the account unchanged, created zero login attempts, and removed password fields after success. Dashboard showed Telegram-only status without iframe/old auth calls; removed current-auth route returned 404.
- Frontend existing suite passed (26 files, 181 tests) and frontend build passed. This does not replace the exercised browser surface above.
- Actual `--check-ai` process against local HTTP fixtures passed synthetic OCR with exactly one completion. A 401 emitted finite `AI_VISION_PREFLIGHT_UNAVAILABLE` / `MODEL_DISCOVERY` / `HTTP_AUTH` / 401 with zero completions and no raw secret.
- Actual blue/green route rendering emitted the exact priority-250 credential route and CSP/no-referrer/no-store middleware with the exercised green frontend/blue gateway targets. Shell syntax and dbtool suite passed; this is not live edge/Docker proof.

**Not observed / prerequisites still missing:**

- Linux Python deployment suite and Docker Compose/image/deploy rehearsal, including actual key-only AI rotation/remount and activation rollback. Native Windows lacks `fcntl`; MSYS noacl reports the 0600 fixture as 0644, so that permission-fixture failure is not a passing Linux suite. Docker/WSL/VPS access is unavailable in this execution.
- Final HTTPS response through production nginx/Traefik/Cloudflare, actual Access OWNER authorization/public-viewer denial and exact-path Rocket Loader/analytics settings.
- Real private Telegram menu/progress/photo/OTP interaction, real vision provider synthetic/privacy acceptance, and owner-authorized live ACB login/exact-account verification/catch-up/logout. No real secrets were read and no production session was disrupted.

For live acceptance, use an owner-selected window or naturally expired session; **do not evict a healthy payment-monitoring session just to test**. Observe a warning then wait/restart safely and establish no login before a fresh button. The owner presses LOGIN, replies to login OTP, and verifies catch-up before realtime readiness. Change only the stored credentials through HTTPS and prove save itself does not log in. Exercise live logout only after the owner's explicit confirmation and report bank/local outcomes separately; unsupported DOM means bank logout remains unaccepted, never an invented success.

The historical harness demonstrated an earlier implementation, including a retry policy that is now removed. Its counts and output are not current end-to-end proof. Keep fixture/UI/race evidence, latest aggregate timing failures, Linux Docker acceptance, live Telegram, live provider and live ACB results separate.
