// Bind the served frontend files and private maps produced by one Docker
// frontend stage. The final runtime image retains only these hashes.
import { createHash } from 'node:crypto';
import { readFile, readdir, writeFile } from 'node:fs/promises';
import { join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

async function hashes(root) {
  const entries = {};
  async function walk(dir) {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const file = join(dir, entry.name);
      if (entry.isDirectory()) await walk(file);
      else if (entry.isFile()) {
        const name = relative(root, file).split(sep).join('/');
        entries[name] = createHash('sha256').update(await readFile(file)).digest('hex');
      } else {
        throw new Error('build contains an unsupported filesystem entry');
      }
    }
  }
  await walk(root);
  return Object.fromEntries(Object.entries(entries).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0));
}

// The browser DSN is injected into the compiled frontend by this same build.
// Bind its project and public ingest key to the image without retaining the
// DSN itself in the runtime manifest. Stage compares this digest with an
// authenticated project read before uploading private source maps.
export function browserDsnBinding(dsn) {
  if (!dsn?.trim()) return '';
  let url;
  try {
    url = new URL(dsn.trim());
  } catch {
    throw new Error('invalid browser DSN build input');
  }
  if (!['http:', 'https:'].includes(url.protocol) || !url.host || url.password ||
      url.search || url.hash || !/^[A-Za-z0-9_-]{1,64}$/.test(url.username) ||
      !/^\/[0-9]{1,20}\/?$/.test(url.pathname)) {
    throw new Error('invalid browser DSN build input');
  }
  const projectID = url.pathname.replace(/\/$/, '').slice(1);
  return createHash('sha256').update(`flare-browser-dsn-v1\n${url.username}\n${projectID}`).digest('hex');
}

export async function writeBuildManifest(publicRoot, privateRoot, output, browserDSN = process.env.PUBLIC_FLARE_DSN ?? '') {
  const publicDir = resolve(publicRoot);
  const privateDir = resolve(privateRoot);
  const outputFile = resolve(output);
  if (publicDir === privateDir || outputFile.startsWith(`${publicDir}${sep}`) ||
      outputFile.startsWith(`${privateDir}${sep}`)) {
    throw new Error('manifest and source maps must stay outside the public build');
  }
  const publicFiles = await hashes(publicDir);
  const sourceMaps = await hashes(privateDir);
  if (!Object.keys(publicFiles).length || !Object.keys(sourceMaps).length) {
    throw new Error('public files and private source maps are required');
  }
  for (const name of Object.keys(sourceMaps)) {
    if (!name.endsWith('.map') || !Object.hasOwn(publicFiles, name.slice(0, -4))) {
      throw new Error('each source map must match a public file');
    }
  }
  const manifest = {
    version: 2,
    public_files: publicFiles,
    source_maps: sourceMaps,
    browser_dsn_binding_sha256: browserDsnBinding(browserDSN)
  };
  await writeFile(outputFile, `${JSON.stringify(manifest)}\n`, { flag: 'w', mode: 0o600 });
  return manifest;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  try {
    if (process.argv.length !== 2) throw new Error('usage: bun write-build-manifest.mjs');
    const manifest = await writeBuildManifest('build', 'source-maps', 'build-manifest.json');
    console.log(`bound ${Object.keys(manifest.public_files).length} public files to ${Object.keys(manifest.source_maps).length} private maps`);
  } catch (error) {
    console.error(error instanceof Error ? error.message : 'build manifest failed');
    process.exitCode = 1;
  }
}
