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

