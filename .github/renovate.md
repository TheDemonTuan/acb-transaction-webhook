# Managed Renovate updates

This repository uses the hosted [Mend Renovate GitHub App](https://github.com/apps/renovate).
There is no self-hosted bot, scheduled Renovate workflow, or repository PAT.

## Activation order

1. Publish the setup branch and open a pull request. The `Pull request checks` workflow runs the reusable CI contract without inheriting deployment secrets or publishing images.
2. Wait for `Dependency Update Gate` to succeed. In GitHub's `main` branch protection, require this exact check, select GitHub Actions as its source, and require the branch to be up to date. Keep the existing administrator enforcement, linear history, and conversation-resolution protections. Do not add a Renovate bypass.
3. Merge the setup pull request only after review. A merge into `main` invokes the existing build and automatic production deployment pipeline; this setup does not bypass it.
4. Install or configure the Renovate App for **only** `TheDemonTuan/acb-transaction-webhook`. Confirm its requested repository permissions, including read access to Dependabot alerts. Use GitHub's installation screen; never put browser credentials or a PAT in the Renovate config.
5. Keep the dependency graph and Dependabot alerts enabled. `.github/dependabot.yml` has been removed and Dependabot security updates are disabled so Mend Renovate is the sole dependency updater PR source.

Do not activate Renovate automerge before the required PR gate is enforced. Native GitHub auto-merge is not required: `platformAutomerge: false` makes Renovate merge PRs itself after passing checks. Renovate uses squash merges and rebases branches behind `main`.

## Update policy

- Stable dependency patch/minor updates and GitHub Actions updates can merge after every PR check succeeds. Pre-1.0 minor updates and all major updates require review.
- Versioned Dockerfile image patch/digest updates can merge after ARM64 image builds, smoke tests and Trivy scans. Initial digest pinning requires review. Docker minor updates require review to avoid unattended runtime-line changes.
- Docker Compose images require review. Edge infrastructure has a separate deployment lifecycle and tests/scripts tied to image versions. A new digest behind a non-versioned tag such as `latest` or `nonroot` also requires review because its update type does not establish compatibility.
- Python source requirements and hashed `.lock` outputs are handled together by the `pip-compile` manager using the existing `uv pip compile` headers. Patch updates can merge after tests; minor/major updates require review. The standalone `pip_requirements` manager is not enabled.
- Bun's version file, package-manager declaration, engine constraint and types are grouped. Minor runtime changes require review. Go runtime minor changes also require review.
- Regular releases wait three days where the datasource supplies a supported release timestamp. Detected vulnerability fixes bypass that wait and the normal PR schedule/limits. They target the lowest fixed version and still obey the same automerge exclusions. Do not set `vulnerabilityAlerts.automerge`: forced vulnerability settings could otherwise override the major-update exclusion.
- OSV alerts supplement GitHub alerts. This Renovate feature is experimental and checks supported direct dependencies only. Do not assume full transitive vulnerability coverage for every manager or lockfile.
- Weekly lockfile maintenance requires review because dependency resolution can change more than one transitive version.

Renovate is not a CVE scanner or proof of compatibility. Trivy blocks HIGH/CRITICAL findings in gateway, worker, dbtool and TTS runtime images, subject to the existing validated CVE allowlist. Tests cannot establish that every patch/minor update is non-breaking. The PR workflow has no `pull_request_target` trigger and receives no production secrets.

## Production exceptions

Production Bark is supplied through the GitHub repository variable `BARK_IMAGE_REF` and approved in `deploy/third-party-allowlist.json`. Renovate does not update GitHub variables or approve new third-party digests. Review and coordinate those changes explicitly; a dev Compose image bump does not update production Bark.

Edge Compose changes are proposals only. Merging them does not deploy the edge stack. Keep coordinated updates to probe scripts, fixtures and image references in the same reviewed change.

OS package updates performed during an image build are not independently tracked by Renovate. Digest updates trigger image rebuilds when upstream changes a tracked tag; this setup does not promise immediate detection of every newly disclosed container CVE.

## Validation

Run from the repository root:

```bash
npx --yes --package renovate@44.173.0 -c 'renovate-config-validator --strict .github/renovate.json && node scripts/verify-renovate-policy.mjs'
python3 -m unittest discover -s deploy/tests -p 'test_dependency_updates.py' -v
bash scripts/verify-actions-pinned.sh
```

The policy check uses the pinned Renovate implementation's package-rule matcher and forced-config merge rather than a separate approximation. It checks ordinary and security updates for major upgrades, pre-1.0 minors, runtime changes, Docker digests, Compose updates and lockfile maintenance. Update the validator version in CI and this command together, then rerun the policy tests.
