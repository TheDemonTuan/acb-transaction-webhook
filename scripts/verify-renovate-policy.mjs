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

const cases = [
  ['npm patch', 'bun', 'npm', 'react', '19.3.0', 'patch', '3 days'],
  ['npm minor', 'bun', 'npm', 'react', '19.3.0', 'minor', '3 days'],
  ['npm major', 'bun', 'npm', 'react', '19.3.0', 'major', '3 days'],
  ['pre-1.0 minor', 'gomod', 'go', 'golang.org/x/sys', '0.48.0', 'minor', '3 days'],
  ['Go patch', 'gomod', 'go', 'example.com/library', '1.2.3', 'patch', '3 days'],
  ['Go runtime patch', 'gomod', 'golang-version', 'go', '1.27.2', 'patch', null],
  ['Go runtime minor', 'gomod', 'golang-version', 'go', '1.27.2', 'minor', '3 days'],
  ['CI Go runtime patch', 'github-actions', 'github-releases', 'actions/go-versions', '1.27.2', 'patch', null],
  ['CI Python runtime patch', 'github-actions', 'github-releases', 'actions/python-versions', '3.13.0', 'patch', null],
  ['CI Node runtime patch', 'github-actions', 'github-releases', 'actions/node-versions', '24.18.0', 'patch', null],
  ['Bun runtime patch', 'bun', 'npm', 'bun', '1.4.3', 'patch', null],
  ['Bun runtime minor', 'bun', 'npm', 'bun', '1.4.3', 'minor', null],
  ['Bun runtime major', 'bun', 'npm', 'bun', '1.4.3', 'major', null],
  ['Bun version file minor', 'custom.regex', 'npm', 'bun', '1.4.3', 'minor', null],
  ['Bun types patch', 'bun', 'npm', '@types/bun', '1.4.3', 'patch', null],
  ['Bun minimum compatibility', 'bun', 'npm', 'bun', '1.4.3', 'minor', null, 'engines', false],
  ['scanner minor', 'custom.regex', 'github-releases', 'aquasecurity/trivy', '0.74.0', 'minor', '3 days'],
  ['Python patch', 'pip-compile', 'pypi', 'fastapi', '0.141.1', 'patch', '3 days'],
  ['Python minor', 'pip-compile', 'pypi', 'fastapi', '0.141.1', 'minor', '3 days'],
  ['Docker patch', 'dockerfile', 'docker', 'python', '3.13.15-slim-trixie', 'patch', null],
  ['Docker digest', 'dockerfile', 'docker', 'python', '3.13.15-slim-trixie', 'digest', null],
  ['edge patch', 'docker-compose', 'docker', 'traefik', 'v3.7.13', 'patch', '3 days'],
  ['action digest', 'github-actions', 'github-tags', 'actions/checkout', '7.0.1', 'digest', '3 days'],
  ['lock maintenance', 'bun', 'npm', 'react', '19.3.0', 'lockFileMaintenance', '3 days'],
];

for (const security of [false, true]) {
  for (const [name, manager, datasource, depName, currentVersion, updateType, expectedAge, depType, expectedEnabled] of cases) {
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
      ...(depType ? { depType } : {}),
      ...(security ? {
        isVulnerabilityAlert: true,
        vulnerabilityFixVersion: '99.0.0',
        force: config.vulnerabilityAlerts,
      } : {}),
    };
    const result = mergeChildConfig(await applyPackageRules(input), {});
    assert.equal(result.automerge, false, `${name} must require operator merge, security=${security}`);
    assert.equal(result.ignoreTests, false, `${name} must not bypass CI`);
    assert.equal(result.minimumReleaseAge, security ? null : expectedAge, `${name} age, security=${security}`);
    if (expectedEnabled !== undefined) assert.equal(result.enabled, expectedEnabled, `${name} must not be updated automatically`);
  }
}
console.log(`Renovate merge policy: ${cases.length * 2} cases passed`);
