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
