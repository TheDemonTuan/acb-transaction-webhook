import { copyFile, lstat, mkdir, readdir, rm } from 'node:fs/promises';
import { dirname, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { BunPlugin } from 'bun';
import postcss from 'postcss';
import tailwindcss from '@tailwindcss/postcss';

const webDirectory = fileURLToPath(new URL('../', import.meta.url));
const distDirectory = resolve(webDirectory, 'dist');
const publicDirectory = resolve(webDirectory, 'public');
const targetCssPath = resolve(webDirectory, 'src/index.css');
const templateHtmlPath = resolve(webDirectory, 'index.html');
const mainEntryPath = resolve(webDirectory, 'src/main.tsx');

const tailwindPlugin: BunPlugin = {
  name: 'tailwind-css',
  setup(build) {
    build.onLoad({ filter: /\.css$/ }, async (args) => {
      if (resolve(args.path) !== targetCssPath) {
        return undefined;
      }
      try {
        const source = await Bun.file(args.path).text();
        const result = await postcss([tailwindcss({ base: webDirectory, optimize: false })]).process(source, {
          from: args.path,
          map: false,
        });
        return {
          contents: result.css,
          loader: 'css',
          resolveDir: dirname(args.path),
        };
      } catch (error) {
        throw new Error(`Failed to compile Tailwind CSS for ${args.path}: ${error instanceof Error ? error.message : String(error)}`);
      }
    });
  },
};

async function copyPublicFiles(sourceDir: string, currentRelDir = '', generatedPaths: Set<string>): Promise<void> {
  const currentDir = resolve(sourceDir, currentRelDir);
  const directoryStat = await lstat(currentDir);
  if (!directoryStat.isDirectory() || directoryStat.isSymbolicLink()) {
    throw new Error(`Not a regular public directory: ${currentRelDir || '.'}`);
  }
  const entries = await readdir(currentDir, { withFileTypes: true });

  for (const entry of entries) {
    const relPath = currentRelDir ? `${currentRelDir}/${entry.name}` : entry.name;
    const sourcePath = resolve(sourceDir, relPath);
    const destPath = resolve(distDirectory, relPath);

    const stat = await lstat(sourcePath);
    if (stat.isSymbolicLink()) {
      throw new Error(`Symlinks are forbidden in public directory: ${relPath}`);
    }

    if (stat.isDirectory()) {
      await mkdir(destPath, { recursive: true });
      await copyPublicFiles(sourceDir, relPath, generatedPaths);
    } else if (stat.isFile()) {
      if (relPath === 'index.html' || relPath === '__release') {
        throw new Error(`Public file would overwrite reserved artifact file: ${relPath}`);
      }
      if (generatedPaths.has(relPath)) {
        throw new Error(`Public file collides with generated bundle output: ${relPath}`);
      }
      await mkdir(dirname(destPath), { recursive: true });
      await copyFile(sourcePath, destPath);
    } else {
      throw new Error(`Not a regular public file: ${relPath}`);
    }
  }
}

async function build(): Promise<void> {
  // Clean only web/dist
  await rm(distDirectory, { recursive: true, force: true });
  await mkdir(distDirectory, { recursive: true });

  const result = await Bun.build({
    entrypoints: [mainEntryPath],
    root: webDirectory,
    outdir: distDirectory,
    target: 'browser',
    format: 'esm',
    splitting: true,
    minify: true,
    sourcemap: 'none',
    env: 'disable',
    define: { 'process.env.NODE_ENV': '"production"' },
    plugins: [tailwindPlugin],
    naming: {
      entry: 'assets/[name]-[hash].[ext]',
      chunk: 'assets/[name]-[hash].[ext]',
      asset: 'assets/[name]-[hash].[ext]',
    },
    publicPath: '/',
  });

  if (!result.success) {
    console.error('Bun.build failed:');
    for (const log of result.logs) {
      console.error(log);
    }
    process.exit(1);
  }

  const generatedPaths = new Set<string>();
  const entryJsOutputs: string[] = [];
  const cssOutputs: string[] = [];

  for (const output of result.outputs) {
    const rel = relative(distDirectory, output.path).replace(/\\/g, '/');
    generatedPaths.add(rel);

    if (output.kind === 'entry-point' && output.path.endsWith('.js')) {
      entryJsOutputs.push(rel);
    }
    if (output.path.endsWith('.css')) {
      cssOutputs.push(rel);
    }
  }

  if (entryJsOutputs.length !== 1) {
    throw new Error(`Expected exactly one entry-point JS output, found ${entryJsOutputs.length}`);
  }
  if (cssOutputs.length === 0) {
    throw new Error('Expected at least one CSS output from build');
  }

  const entryJsUrl = `/${entryJsOutputs[0]}`;
  const cssUrls = cssOutputs.map((css) => `/${css}`);

  const templateHtml = await Bun.file(templateHtmlPath).text();
  let moduleEntryFound = 0;

  const rewriter = new HTMLRewriter()
    .on('head', {
      element(el) {
        for (const cssUrl of cssUrls) {
          el.append(`    <link rel="stylesheet" href="${cssUrl}" />\n  `, { html: true });
        }
      },
    })
    .on('script[type="module"]', {
      element(el) {
        if (el.getAttribute('src') === '/src/main.tsx') {
          el.setAttribute('src', entryJsUrl);
          moduleEntryFound++;
        }
      },
    });

  const rewrittenHtml = rewriter.transform(templateHtml);

  if (moduleEntryFound !== 1) {
    throw new Error(`Expected exactly one module script targeting /src/main.tsx in template, found ${moduleEntryFound}`);
  }

  await Bun.write(resolve(distDirectory, 'index.html'), rewrittenHtml);

  // Copy public files into dist
  await copyPublicFiles(publicDirectory, '', generatedPaths);

  console.log(`Successfully built production artifact in ${distDirectory}`);
}

try {
  await build();
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
}
