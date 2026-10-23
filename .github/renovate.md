# Renovate dependency updates

The active policy is `.github/renovate-oss.json`, read by the unmodified Renovate OSS runner in [TheDemonTuan/renovate-control](https://github.com/TheDemonTuan/renovate-control) through the administrator's `configFileNames` setting. `.github/renovate.json` contains only `enabled: false`: it disables the previous Mend-hosted bot and prevents re-onboarding. Do not delete this opt-out while the old GitHub App remains installed. The new App is `thedemontuan-renovate-oss`; it uses the separate `rcc/` branch namespace so hosted cleanup cannot touch its dependency branches.

## Update policy

- Renovate proposes the newest suitable stable release, respecting npm's `latest` distribution tag and repository constraints. Declared dependency ranges use `rangeStrategy: bump`; this is not proof of source or runtime compatibility.
- All ACB updates require operator merge, including security, patch, minor, Docker digest and Actions updates. Do not use `ignoreTests: true`. A merge into `main` can invoke the existing automatic production deployment pipeline; this policy does not bypass or change it.
- Ordinary library/action releases wait three days when the datasource provides a supported release timestamp. Detected vulnerability fixes, Bun toolchain releases and known Go/CI runtime patch releases bypass the ordinary delay.
- Bun's `.bun-version`, `packageManager`, and published `@types/bun` versions are grouped into a toolchain PR without a release-age wait. Types are selected from actual registry releases, not forced to match the runtime number. `engines.bun` is excluded: raising minimum compatibility is a deliberate application decision.
- Python source requirements and hashed lockfiles remain managed together by `pip-compile` using the existing `uv pip compile` headers. The standalone `pip_requirements` manager remains disabled.
- Go indirect security updates, Dockerfiles, Compose, Actions SHA pins, Trivy version declarations and the Renovate validator version remain managed.
- Weekly lockfile maintenance requires review because package-manager resolution can change multiple transitive dependencies.
- OSV supplements GitHub alerts for supported direct dependencies. It is experimental and does not provide complete transitive/container vulnerability coverage.

The local Dockerfile patch/digest release-age exception is not evidence that every proposed image update fixes a CVE. The centralized runner can apply stricter administrator constraints over this repository policy.

## Cutover and safety

1. Review this configuration PR and require `Dependency Update Gate` from GitHub Actions, strict/up-to-date branch protection and administrator enforcement. Do not grant the bot a branch-protection bypass.
2. Install the private fleet GitHub App and complete an ACB dry-run before enabling write runs.
3. Keep the hosted opt-out disabled before the first OSS write run. Close only the previous `renovate[bot]` PRs/branches and its Dependency Dashboard after review. Removing the old App's repository access in GitHub installation settings is optional additional permission cleanup; a repository config does not revoke its permissions.
4. Keep dependency graph and Dependabot alerts enabled. Keep duplicate Dependabot version/security PR automation disabled; alerts are a separate feature.
5. Run Renovate OSS through the central workflow and verify its real PR, lockfile and full CI results. Dispatch acceptance alone is not successful dependency updating.

The PR workflow uses no `pull_request_target` trigger and receives no production secrets. Trivy blocks HIGH/CRITICAL findings in runtime images subject to the validated allowlist. Green CI cannot establish that every patch/minor update is non-breaking.

## Production exceptions

Production Bark uses the GitHub variable `BARK_IMAGE_REF` and the approved `deploy/third-party-allowlist.json`. Renovate does not update GitHub variables or approve digests. A development Compose bump does not update production Bark.

Edge Compose changes are proposals; merging them does not deploy the edge stack. Coordinate probe scripts, fixtures and image references in the same reviewed change. OS packages installed during image builds are not independently updated by Renovate; source/lockfile remediation is not proof the fixed image was deployed.

## Validation

From the repository root:

```bash
npx --yes --package renovate@44.178.5 -c 'renovate-config-validator --strict .github/renovate.json .github/renovate-oss.json && node scripts/verify-renovate-policy.mjs'
bash scripts/verify-actions-pinned.sh
```

The verifier uses Renovate's actual package-rule matcher/forced merge to check ordinary/security updates, release-age exceptions, operator-only merging, and preservation of `engines.bun`. The validator version in CI is itself tracked by a Renovate regex manager. The central runner has its own pinned-engine safety tests.

The App-authenticated [ACB dry-run](https://github.com/TheDemonTuan/renovate-control/actions/runs/39926740102) extracted 237 dependency declarations across seven managers before cutover. After splitting the configs, a local engine run selected `.github/renovate-oss.json`, found the Bun 1.4.4 updates and kept `engines.bun` disabled. The same engine without the custom filename returned `disabled-by-config`, exercising the hosted opt-out. Neither dry-run is evidence that a dependency PR has merged or a CVE has been resolved.
