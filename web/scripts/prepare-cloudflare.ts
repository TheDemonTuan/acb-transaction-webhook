import { lstat, readdir } from 'node:fs/promises';
import { extname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Post-process Vite output only. Cloudflare publishing runs Wrangler under Node,
// not Bun; this script intentionally uses Bun's file and HTML APIs.
const webDirectory = fileURLToPath(new URL('../', import.meta.url));
const distDirectory = resolve(webDirectory, 'dist');
const publicDirectory = resolve(webDirectory, 'public');
const maxRules = 100;
const maxHeaderLine = 2_000;
const cloudflareMaxBytes = 25 * 1024 * 1024;
// Set a smaller ceiling if the account's asset limit is below 25 MiB.
const configuredLimit = process.env.CLOUDFLARE_ASSET_MAX_BYTES;
const maxAssetBytes = configuredLimit === undefined ? cloudflareMaxBytes : Number(configuredLimit);
const assetExtensions: Record<string, true> = {
  '.js': true, '.css': true, '.svg': true, '.png': true, '.jpg': true, '.jpeg': true,
  '.gif': true, '.webp': true, '.avif': true, '.ico': true, '.woff': true, '.woff2': true,
  '.ttf': true, '.otf': true, '.eot': true, '.mp3': true, '.wav': true, '.ogg': true,
  '.m4a': true, '.mp4': true, '.webm': true, '.wasm': true,
};
const fingerprint = /^.+-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$/;
const files = new Set<string>();
const generatedAssets = new Set<string>();

function fail(message: string): never {
  throw new Error(message);
}

async function inspectDirectory(directory: string, prefix = ''): Promise<void> {
  if (!(await lstat(directory)).isDirectory()) fail(`Not a regular directory: dist/${prefix}`);
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const relative = `${prefix}${entry.name}`;
    // Besides excluding hidden/source/environment files, this ensures generated
    // _headers paths cannot contain wildcards, placeholders, or line breaks.
    if (!/^[A-Za-z0-9_-][A-Za-z0-9._/-]*$/.test(relative) || entry.name.startsWith('.')) {
      fail(`Unsafe artifact path: ${relative}`);
    }
    const absolute = resolve(distDirectory, relative);
    const stat = await lstat(absolute);
    if (stat.isSymbolicLink()) fail(`Symlinks are forbidden in the artifact: ${relative}`);
    if (stat.isDirectory()) {
      if (relative !== 'assets' && !relative.startsWith('assets/')) {
        fail(`Only compiled assets may have artifact subdirectories: ${relative}`);
      }
      await inspectDirectory(absolute, `${relative}/`);
      continue;
    }
    if (!stat.isFile()) fail(`Not a regular artifact file: ${relative}`);
    if (stat.size > maxAssetBytes) fail(`Asset exceeds ${maxAssetBytes} bytes: ${relative}`);
    const extension = extname(relative).toLowerCase();
    // Fail closed instead of uploading .map, sources, .env, JSON/CSV bank data,
    // credentials, archives, or arbitrary files copied into public/.
    if (!['index.html', '_headers', '__release'].includes(relative) && !Object.hasOwn(assetExtensions, extension)) {
      fail(`Prohibited or unsupported artifact file: ${relative}`);
    }
    const generated = relative.startsWith('assets/') && fingerprint.test(entry.name)
      && !(await Bun.file(resolve(publicDirectory, relative)).exists());
    if (extension === '.js' && !generated) {
      fail(`JavaScript must be a fingerprinted Vite output, not copied source: ${relative}`);
    }
    if (generated) generatedAssets.add(relative);
    files.add(relative);
  }
}

const referenceOrigin = 'https://artifact.invalid';
function localReference(reference: string, from: string): string | undefined {
  if (reference.startsWith('data:') || reference.startsWith('blob:') || reference.startsWith('#')) return;
  const url = new URL(reference, `${referenceOrigin}/${from}`);
  if (url.origin !== referenceOrigin) return;
  const path = decodeURIComponent(url.pathname).slice(1);
  if (!files.has(path)) fail(`Missing asset referenced by ${from}: ${path}`);
  return path;
}

async function inspectReferences(): Promise<void> {
  if (!files.has('index.html')) fail('Missing dist/index.html');
  const html = await Bun.file(resolve(distDirectory, 'index.html')).text();
  let hasRoot = false;
  let moduleEntries = 0;
  const rewriter = new HTMLRewriter()
    .on('#root', { element() { hasRoot = true; } })
    .on('script[src]', {
      element(element) {
        const source = element.getAttribute('src')!;
        const path = localReference(source, 'index.html');
        if (element.getAttribute('type') === 'module') {
          if (!path || extname(path) !== '.js' || !generatedAssets.has(path)) {
            fail('Entry module must reference a fingerprinted local JavaScript asset');
          }
          moduleEntries++;
        }
      },
    })
    .on('link[href]', {
      element(element) {
        const relationship = (element.getAttribute('rel') ?? '').toLowerCase().split(/\s+/);
        if (!relationship.some((value) => ['stylesheet', 'modulepreload', 'preload', 'icon'].includes(value))) return;
        const path = localReference(element.getAttribute('href')!, 'index.html');
        if (relationship.includes('stylesheet') && (!path || extname(path) !== '.css')) {
          fail('Stylesheet entry must reference a local CSS asset');
        }
      },
    });
  for (const [selector, attribute] of [['img[src]', 'src'], ['source[src]', 'src'], ['video[src]', 'src'], ['audio[src]', 'src'], ['video[poster]', 'poster']]) {
    rewriter.on(selector, {
      element(element) { localReference(element.getAttribute(attribute)!, 'index.html'); },
    });
  }
  await rewriter.transform(new Response(html)).text();
  if (!hasRoot || moduleEntries === 0) fail('index.html must contain the root mount and an entry module');

  const transpiler = new Bun.Transpiler({ loader: 'js' });
  for (const path of files) {
    const extension = extname(path).toLowerCase();
    if (extension !== '.js' && extension !== '.css') continue;
    const source = await Bun.file(resolve(distDirectory, path)).text();
    if (/sourceMappingURL\s*=/.test(source)) fail(`Source maps must not be published: ${path}`);
    if (extension === '.js') {
      for (const dependency of transpiler.scanImports(source)) {
        // Bundles must not depend on a bare npm import or an external module.
        if (!dependency.path.startsWith('.') && !dependency.path.startsWith('/')) {
          fail(`Unbundled or external JavaScript import in ${path}`);
        }
        localReference(dependency.path, path);
      }
      // Vite also emits URL assets and preload dependencies as string literals
      // rather than import declarations. Resolve those against the containing file.
      for (const match of source.matchAll(/["'`]((?:\/assets\/|assets\/|\.\.?\/)[^"'`\s\\]+\.(?:js|css|svg|png|jpe?g|gif|webp|avif|ico|woff2?|ttf|otf|eot|mp3|wav|ogg|m4a|mp4|webm|wasm)(?:[?#][^"'`\s\\]*)?)["'`]/g)) {
        const reference = match[1];
        localReference(reference.startsWith('assets/') ? `/${reference}` : reference, path);
      }
    } else {
      for (const match of source.matchAll(/url\(\s*(?:"([^"]*)"|'([^']*)'|([^\s)]*))\s*\)|@import\s+(?:"([^"]*)"|'([^']*)')/g)) {
        localReference(match[1] ?? match[2] ?? match[3] ?? match[4] ?? match[5], path);
      }
    }
  }
}

function headerRuleCount(headers: string): number {
  let rules = 0;
  for (const line of headers.split(/\r?\n/)) {
    if (line.length > maxHeaderLine) fail('_headers exceeds the 2,000-character line limit');
    if (!line.trim() || line.trimStart().startsWith('#')) continue;
    if (!/^\s/.test(line)) rules++;
  }
  return rules;
}

async function prepare(): Promise<void> {
  const sha = process.env.RELEASE_COMMIT;
  if (!sha || !/^[0-9a-f]{40}$/.test(sha)) fail('RELEASE_COMMIT must be exactly 40 lowercase hexadecimal characters');
  if (!Number.isSafeInteger(maxAssetBytes) || maxAssetBytes < 1 || maxAssetBytes > cloudflareMaxBytes) {
    fail('CLOUDFLARE_ASSET_MAX_BYTES must be an integer from 1 to 26214400');
  }
  await inspectDirectory(distDirectory);
  await inspectReferences();

  const baseHeaders = (await Bun.file(resolve(publicDirectory, '_headers')).text()).trimEnd();
  const baseRuleCount = headerRuleCount(baseHeaders);
  if (baseRuleCount > maxRules) fail('Security _headers rules exceed Cloudflare\'s 100-rule limit');
  const immutablePaths = [...generatedAssets].sort();
  let headers = `${baseHeaders}\n`;
  if (baseRuleCount + immutablePaths.length > maxRules) {
    console.warn(`Cloudflare _headers would need ${baseRuleCount + immutablePaths.length} rules (limit ${maxRules}); retaining security rules and omitting ALL immutable rules. Assets will revalidate.`);
  } else {
    for (const path of immutablePaths) {
      headers += `\n/${path}\n  Cache-Control: public, max-age=31536000, immutable\n`;
    }
  }
  headerRuleCount(headers);
  const release = `${sha}\n`;
  if (new TextEncoder().encode(headers).byteLength > maxAssetBytes || release.length > maxAssetBytes) {
    fail('Generated metadata exceeds the configured asset size limit');
  }
  // Publish identity only after every artifact check has passed. Never generate
  // /healthz or /readyz: those paths remain exclusively owned by the backend.
  await Bun.write(resolve(distDirectory, '_headers'), headers);
  await Bun.write(resolve(distDirectory, '__release'), release);
  console.log(`Prepared Cloudflare static artifact for ${sha} (${files.size} existing files, ${headerRuleCount(headers)} header rules).`);
}

try {
  await prepare();
} catch (error) {
  console.error(`Cloudflare artifact preparation failed: ${error instanceof Error ? error.message : 'unknown error'}`);
  process.exitCode = 1;
}
