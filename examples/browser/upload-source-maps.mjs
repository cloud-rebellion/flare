// CI-only uploader for the private maps extracted from the exact browser
// bundle being deployed. It never puts a member key or map into the bundle.
import { readFile, readdir, stat } from 'node:fs/promises';
import { join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { sourceMapNameForURL } from './source-map-name.mjs';

const maxMapBytes = 29 << 20; // leave room for JSON under the 30 MiB API limit

async function mapFiles(root) {
  const found = [];
  async function walk(dir) {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const file = join(dir, entry.name);
      if (entry.isDirectory()) await walk(file);
      else if (entry.isFile() && entry.name.endsWith('.js.map')) found.push(file);
    }
  }
  await walk(root);
  return found.sort();
}

export async function uploadBrowserSourceMaps({ mapsDir, publicBase, flareBase, projectID, release, memberKey, fetcher = fetch }) {
  const assets = new URL(publicBase);
  const collector = new URL(flareBase);
  if (!['http:', 'https:'].includes(assets.protocol) || !assets.pathname.endsWith('/')) {
    throw new Error('publicBase must be an HTTP(S) directory URL ending in /');
  }
  if (collector.protocol !== 'https:' || collector.username || collector.password ||
      collector.pathname !== '/' || collector.search || collector.hash) {
    throw new Error('flareBase must be an HTTPS origin');
  }
  if (!/^[A-Za-z0-9_-]{1,64}$/.test(projectID) || !release || !memberKey) {
    throw new Error('project ID, release, and member key are required');
  }
  const root = resolve(mapsDir);
  const files = await mapFiles(root);
  if (files.length === 0) throw new Error('no JavaScript source maps found');
  const receipts = [];
  for (const file of files) {
    const size = (await stat(file)).size;
    if (size > maxMapBytes) throw new Error('source map exceeds safe upload size');
    const rel = relative(root, file).split(sep).join('/');
    const scriptURL = new URL(rel.slice(0, -4), assets);
    const name = sourceMapNameForURL(scriptURL.href);
    const body = JSON.stringify({ release, name, content: await readFile(file, 'utf8') });
    if (Buffer.byteLength(body, 'utf8') >= (30 << 20)) throw new Error('source map request exceeds API upload limit');
    const response = await fetcher(new URL(`/api/projects/${encodeURIComponent(projectID)}/artifacts`, collector), {
      method: 'POST',
      headers: { Authorization: `Bearer ${memberKey}`, 'Content-Type': 'application/json' },
      body
    });
    if (response.status !== 201) throw new Error(`source-map upload failed (HTTP ${response.status})`);
    const receipt = await response.json();
    if (receipt.parse_status !== 'valid' || receipt.name !== name || receipt.release !== release || !receipt.id) {
      throw new Error('source-map upload did not return a valid matching artifact receipt');
    }
    receipts.push({ id: receipt.id, name, release });
  }
  return receipts;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  try {
    if (process.argv.length !== 7) {
      throw new Error('usage: bun upload-source-maps.mjs <maps-dir> <public-base-url/> <flare-https-origin> <project-id> <release>');
    }
    const receipts = await uploadBrowserSourceMaps({
      mapsDir: process.argv[2], publicBase: process.argv[3], flareBase: process.argv[4],
      projectID: process.argv[5], release: process.argv[6], memberKey: process.env.FLARE_MEMBER_KEY
    });
    console.log(`accepted ${receipts.length} source maps for release ${receipts[0].release}`);
    for (const receipt of receipts) console.log(`${receipt.id} ${receipt.name}`);
  } catch (error) {
    // Do not log the member key, source code, script URL, or server response.
    console.error(error instanceof Error ? error.message : 'source-map upload failed');
    process.exitCode = 1;
  }
}
