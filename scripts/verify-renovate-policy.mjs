import assert from 'node:assert/strict';
import { readFile, realpath, access } from 'node:fs/promises';
import { delimiter, dirname, join } from 'node:path';
import { pathToFileURL } from 'node:url';

const config = JSON.parse(await readFile('.github/renovate.json', 'utf8'));
let renovateRoot;
for (const directory of process.env.PATH.split(delimiter)) {
  const binary = join(directory, 'renovate-config-validator');
  try {
    await access(binary);
    renovateRoot = dirname(dirname(await realpath(binary)));
    break;
  } catch {
    // npm exec exposes the pinned validator through PATH, not project dependencies.
  }
}
assert.ok(renovateRoot, 'Run this check with npm exec --package renovate');
const { applyPackageRules } = await import(pathToFileURL(join(
  renovateRoot, 'dist/util/package-rules/index.js',
)));
const { mergeChildConfig } = await import(pathToFileURL(join(
  renovateRoot, 'dist/config/utils.js',
)));

assert.equal(config.automerge, false);
assert.equal(config.platformAutomerge, false);
assert.equal(config.ignoreTests, false);
assert.equal(config.automergeType, 'pr');
assert.equal(config.automergeStrategy, 'squash');
assert.equal(config.vulnerabilityAlerts.automerge, undefined,
  'Forced vulnerability settings must not override the per-update merge policy');

const cases = [
  ['npm patch', 'bun', 'npm', 'react', '19.3.0', 'patch', true, '3 days'],
  ['npm minor', 'bun', 'npm', 'react', '19.3.0', 'minor', true, '3 days'],
  ['npm major', 'bun', 'npm', 'react', '19.3.0', 'major', false, '3 days'],
  ['pre-1.0 minor', 'gomod', 'go', 'golang.org/x/sys', '0.48.0', 'minor', false, '3 days'],
  ['Go patch', 'gomod', 'go', 'example.com/library', '1.2.3', 'patch', true, '3 days'],
  ['Go runtime patch', 'gomod', 'golang-version', 'go', '1.27.2', 'patch', true, null],
  ['Go runtime minor', 'gomod', 'golang-version', 'go', '1.27.2', 'minor', false, '3 days'],
  ['CI Go runtime patch', 'github-actions', 'github-releases', 'actions/go-versions', '1.27.2', 'patch', true, null],
  ['CI Go runtime minor', 'github-actions', 'github-releases', 'actions/go-versions', '1.27.2', 'minor', false, '3 days'],
  ['CI Python runtime patch', 'github-actions', 'github-releases', 'actions/python-versions', '3.13.0', 'patch', true, null],
  ['CI Python runtime minor', 'github-actions', 'github-releases', 'actions/python-versions', '3.13.0', 'minor', false, '3 days'],
  ['CI Node runtime patch', 'github-actions', 'github-releases', 'actions/node-versions', '24.18.0', 'patch', true, null],
  ['CI Node runtime minor', 'github-actions', 'github-releases', 'actions/node-versions', '24.18.0', 'minor', false, '3 days'],
  ['Bun runtime minor', 'bun', 'npm', 'bun', '1.4.3', 'minor', false, '3 days'],
  ['Bun version file minor', 'custom.regex', 'npm', 'bun', '1.4.3', 'minor', false, '3 days'],
  ['Bun types minor', 'bun', 'npm', '@types/bun', '1.4.3', 'minor', false, '3 days'],
  ['scanner minor', 'custom.regex', 'github-releases', 'aquasecurity/trivy', '0.74.0', 'minor', false, '3 days'],
  ['Python patch', 'pip-compile', 'pypi', 'fastapi', '0.141.1', 'patch', true, '3 days'],
  ['Python minor', 'pip-compile', 'pypi', 'fastapi', '0.141.1', 'minor', false, '3 days'],
  ['Docker patch', 'dockerfile', 'docker', 'python', '3.13.15-slim-trixie', 'patch', true, null],
  ['Docker runtime minor', 'dockerfile', 'docker', 'python', '3.13.15-slim-trixie', 'minor', false, '3 days'],
  ['Docker digest', 'dockerfile', 'docker', 'python', '3.13.15-slim-trixie', 'digest', true, null],
  ['Docker initial pin', 'dockerfile', 'docker', 'python', '3.13.15-slim-trixie', 'pinDigest', false, '3 days'],
  ['Distroless digest', 'dockerfile', 'docker', 'gcr.io/distroless/static-debian12', 'nonroot', 'digest', false, null],
  ['Bark latest digest', 'docker-compose', 'docker', 'ghcr.io/finb/bark-server', 'latest', 'digest', false, '3 days'],
  ['edge digest', 'docker-compose', 'docker', 'traefik', 'v3.7.13', 'digest', false, '3 days'],
  ['edge patch', 'docker-compose', 'docker', 'traefik', 'v3.7.13', 'patch', false, '3 days'],
  ['action digest', 'github-actions', 'github-tags', 'actions/checkout', '7.0.1', 'digest', true, '3 days'],
  ['action major', 'github-actions', 'github-tags', 'actions/checkout', '7.0.1', 'major', false, '3 days'],
  ['lock maintenance', 'bun', 'npm', 'react', '19.3.0', 'lockFileMaintenance', false, '3 days'],
];

for (const security of [false, true]) {
  for (const [name, manager, datasource, depName, currentVersion, updateType, expected, expectedAge] of cases) {
    const input = {
      ...config,
      manager,
      datasource,
      depName,
      packageName: depName,
      currentVersion,
      currentValue: currentVersion,
      versioning: datasource === 'docker' ? 'docker' : 'semver',
      updateType,
      ...(security ? {
        isVulnerabilityAlert: true,
        vulnerabilityFixVersion: '99.0.0',
        force: config.vulnerabilityAlerts,
      } : {}),
    };
    const result = mergeChildConfig(await applyPackageRules(input), {});
    assert.equal(result.automerge, expected, `${name}, security=${security}`);
    assert.equal(result.minimumReleaseAge, security ? null : expectedAge, `${name} age, security=${security}`);
  }
}
console.log(`Renovate merge policy: ${cases.length * 2} cases passed`);
