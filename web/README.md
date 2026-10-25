# Frontend toolchain

Use Bun **1.4.3**, pinned in `.bun-version`. From `web/`:

```sh
bun ci
bun run typecheck
bun run build
bun run test
bun run e2e
```

`bun check` checks source, unit tests, Vite configuration and `scripts/` without emitting JavaScript. Unit tests use `bun:test`; `bunfig.toml` excludes Playwright E2E specs. Tests replacing browser globals must use `tests/test-globals.ts` and restore them after each test.

Voice-profile tests must import `VoiceProviderMode` with `import type` (or a named `type` import); test-only type references are checked by the production build too.

## Development and production

`bun run dev` runs the locked Vite CLI on Bun at `127.0.0.1:5173`, preserving React Fast Refresh, CSP headers and gateway proxies at port 18081. Vite is not used for production builds.

`bun run build` typechecks and executes `scripts/build.ts`. Bun bundles the browser entry, with Tailwind 4.3.3 compiled through its official PostCSS integration. Only generated `dist/` is cleared. Public files are copied without following symlinks or overwriting generated/reserved files.

For an immutable Cloudflare artifact:

```sh
RELEASE_COMMIT=<40-character-commit-sha> bun run build:cloudflare
node node_modules/wrangler/bin/wrangler.js deploy --dry-run
```

The postprocessor validates asset references, fingerprints, allowed file types, size limits, security/cache headers and release identity before packaging. Do not bypass it or rebuild an already verified release before packaging.

## Tool runtime exception

Playwright is invoked through its installed CLI with Bun. Its default E2E configuration starts a fixture gateway and Vite, covers desktop/mobile Chromium, and resets `/tmp/tbg-playwright` on Linux; never put real data there.

Wrangler **retains Node 24**, including the pinned frontend deployment setup-node action. Bun 1.4.3 ran Wrangler's dry-run and started its local server, but HTTP requests hung. The same artifact under Node passed the HTTP verifier and guest-payment desktop/mobile suite. There is no automatic runtime fallback. Cloudflare's runtime remains workerd; GitHub Actions may also use Node internally.

Operational allowlist validators use Python3 only and do not require Bun or Node. PowerShell E2E execution requires separate Windows verification.

## SePay balance and Bark rollout

Deploy the backend before copying the updated Telegram template from `/admin/connection`. On my.sepay.vn, insert `balance={{accumulated}}` immediately after `reference={{reference_number}}` and before `content={{transaction_content}}`. Balance is optional: if the bank/package does not provide it or the variable does not render, remove the entire `balance=` line. Existing V1 templates without balance still record valid credits. Native webhooks already supply `accumulated`; do not change transport just to obtain balance.

Bark shows the credit amount first, balance in the subtitle and customer transfer content first in the body, subject to the channel's visibility switches. Balance is SePay's snapshot at that transaction, including retries and catch-up, not a live bank balance or a separate VA balance. Missing/unusable balance (including SePay's ambiguous zero) displays “chưa có dữ liệu”. Private balance and original transfer content are encrypted per transaction for Bark; they are not added to shared events, journal, public transaction descriptions or custom webhook payloads. Historical events without a snapshot show missing information rather than invented data.

New channels default to group `Giao dịch`, no custom icon, and balance/content enabled. Existing saved channel revisions and explicit false values are preserved; deploying does not update or enable existing channels. `BARK_DEFAULT_GROUP` is not a mechanism for rewriting saved channel configuration.

After deployment, the Owner must update only the iPhone channel actually in use at `/admin/notifications`: enable balance/content, use group `SePay`, and clear a legacy ACB icon if present. Keep its device key, sound, priority, custom URL and current enabled/disabled state. Save through the normal edit form to create a new revision. Do not enable disabled channels or change other channels. If multiple channels cannot be identified, the Owner selects by name and sends a test; never bulk-update devices.

Balance/content may be visible on the lock screen when iPhone Show Previews is enabled. The Owner chooses this privacy setting and confirms Bark notification/banner permission and that the selected group is not muted. Send the marked `THỬ` sample from the selected channel; inspect the compact banner, Notification Center and expanded notification, then try long content, large Dynamic Type, catch-up and disabled previews. Test pushes have no fake transaction link; a real transaction should open its existing detail route. Browser/HTTP tests verify payload and web forms only, not APNs or iOS layout. A real bank balance comparison requires an Owner-initiated small transfer after updating the template; never initiate transfers or mass replay production transactions automatically.

For local browser checks, use a newly created temporary data directory with the fixture gateway and Vite development ports documented above. Do not run the default Playwright startup command against existing data: it resets `/tmp/tbg-playwright`. Prefer a separately started isolated fixture and `E2E_SKIP_WEBSERVER=1` when invoking focused E2E tests.

## Transaction voice audio

Voice announcements use only **VieNeu · Mai Anh · nữ miền Bắc**, at rate 1 and pitch 0. Device/browser speech and other provider selectors are removed. Existing enabled, volume, hidden-tab and unprocessed template preferences remain intact; obsolete browser voice identifiers are not saved again. Changing this release does not turn audio on or raise a user's saved volume.

The unlock button creates/resumes the Web Audio context from a user gesture and preloads and decodes the bundled chime before reporting success. If fetching or decoding the recording fails, unlock returns false with `CHIME_ASSET_UNAVAILABLE` and audio stays unavailable; use the unlock control again to retry. Announcements never fetch the chime on demand. If the browser suspends the context, the current operation stops and the UI asks for another unlock; it does not replay skipped history. Only eligible fresh realtime credits enter the existing FIFO/dedupe flow.

The chime is the bundled, unchanged **Calm** recording by **Aarni Koskela (akx)**, licensed under [CC BY 3.0](https://creativecommons.org/licenses/by/3.0/) ([legal code](https://creativecommons.org/licenses/by/3.0/legalcode)). It comes from [`akx/Notifications` at commit `db184d33834a94028b6600d155d312ce7b704e8d`](https://github.com/akx/Notifications/blob/db184d33834a94028b6600d155d312ce7b704e8d/WAV/Calm.wav); the [pinned creator README](https://github.com/akx/Notifications/blob/db184d33834a94028b6600d155d312ce7b704e8d/README.md) provides the license choice. Attribution and provenance are also recorded in `src/features/voice-announcements/assets/Calm.LICENSE.txt` and shown next to the chime setting. The complete recording lasts approximately **1.655465 seconds** (1.6554648526077098 seconds) and plays once at its original rate through **4× clamped device-volume gain**. The source sample peak of 0.14459228515625 scales to 0.578369140625 at full volume, below full scale; this is source PCM arithmetic, not a browser-render result. There is no oscillator fallback, looping, padding, stretching, or audio-file modification; only playback volume is scaled. All speech transports retain the unchanged 2.5× input-gain/soft-peak-protection/device-volume graph. This is not a guarantee of perceived loudness on every speaker; speech near full scale is softly saturated rather than amplified into hard clipping. Mute remains mute.

Stream download completion is separate from `canplay` readiness. Both must complete within the existing preparation budget; a completed body alone does not permit playback. Cold synthesis uses the same VieNeu profile, never a different engine or an automatic retry. The playback watchdog remains 120 seconds.

The focused real-audio browser specs use the retained VieNeu manifest/MP3 clips, actual Web Audio/media decoding, and an OfflineAudioContext waveform harness. They do not establish production deployment health or audible output on an unavailable physical device. Run them against an isolated fixture gateway/Vite instance, never existing application data:

```powershell
$env:E2E_SKIP_WEBSERVER='1'
$env:E2E_BASE_URL='http://127.0.0.1:5173'
bun run e2e -- voice-announcements.spec.ts voice-owner-lifecycle.spec.ts voice-real-audio.spec.ts
```

`voice-real-audio.spec.ts` is designed to attach JSON waveform/media evidence and RMS logs. The recording harness loads and decodes the bundled WAV using the production loader and renders the production scheduler in an OfflineAudioContext. Assertions cover the 1.6554648526077098-second encoded duration within sample tolerance, finite samples on every channel, zero full-scale samples, headroom and energy, sample identity against the decoded asset multiplied by 4× clamped volume, unchanged active waveform extent, full-volume RMS/peak amplitude ratios of 4, half-volume amplitude ratios of 0.5 relative to full volume, silence when muted, and silence after the encoded end. Unit coverage addresses recording loading, ownership, full duration, cleanup, and parallel preparation, with final-partial and cancellation timing paths; gain is checked through rendered samples rather than wiring-only gain-value assertions. Speech waveform assertions retain low-level sine amplitude ratios of 2.5 at full device volume and 1.25 at half volume (±1%), silence when muted, and unchanged soft-peak protection. The spec also decodes all 140 retained pack clips, requires finite processed samples and peaks no greater than 0.892 with unchanged duration, and forces the compressed-pack route using a pinned 16 MiB decoded buffer. The fixtures read the committed `b5a2b7b8dec26c5ec21c3f8a7d91fa33ee75bcc587b51f444a6dbcb6bb34fcf0` manifest and its SHA-256-addressed MP3 files directly from `tts-gateway/assets/voicepacks/`. These are coverage descriptions, not results: no local tests, builds, or physical listening checks were performed for this recording cutover or gain adjustment.

Historical local Windows verification (2026-10-24, Bun 1.4.4 and Playwright Chromium 153), before the adjustment from the 850 ms chime and 2× speech gain, passed 96 focused unit tests, `bun run typecheck` (111 files), all 260 unit tests, and all 22 desktop/mobile Chromium cases across the three voice specs above. These results describe the pre-adjustment audio, not verification of the current Calm recording or 2.5× speech gain; no current-chime waveform result is implied. That browser coverage included native decoded-pack, cold-stream and compressed-pack playback, a suspended context requiring another explicit unlock, realtime dedupe/history exclusion, and rendering all 140 retained clips. Mobile Chromium emulation is not Safari/iPhone verification; physical speaker/headphone listening remains unverified, and no production deployment is claimed.

To retain passing screenshots, telemetry, waveform JSON and RMS logs, use a file-producing reporter (the default terminal reporter does not retain passing in-memory attachments):

```powershell
$env:PLAYWRIGHT_HTML_OPEN='never'
$env:PLAYWRIGHT_JSON_OUTPUT_FILE=Join-Path $PWD 'test-results/vieneu-audio-report.json'
bun run e2e -- voice-announcements.spec.ts voice-owner-lifecycle.spec.ts voice-real-audio.spec.ts --reporter=dot,json,html
```

Keep the isolated fixture processes running only for the check, then stop those processes. `playwright-report/index.html` holds the settings screenshots and audio evidence; `test-results/vieneu-audio-report.json` is the machine-readable report. The transport fixtures use committed MP3 bytes, not live inference: gateway/model inference remains a separate container smoke gate.


