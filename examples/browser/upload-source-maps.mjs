// CI-only uploader for the private maps extracted from the exact browser
// bundle being deployed. It never puts a member key or map into the bundle.
import { readFile, readdir, stat } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { sourceMapNameForURL } from './source-map-name.mjs';

const maxMapBytes = 30 << 20;
const maxBundleBody = 64 << 20;
const maxBundleMaps = 512;
const compareNames = (a, b) => Buffer.compare(Buffer.from(a.name, 'utf8'), Buffer.from(b.name, 'utf8'));

export function sourceMapBundleDigest(release, artifacts) {
  const hash = createHash('sha256');
  hash.update('flare-source-map-bundle-v1\0');
  const field = (value) => {
    const bytes = Buffer.from(value, 'utf8');
    const length = Buffer.alloc(8);
    length.writeBigUInt64BE(BigInt(bytes.length));
    hash.update(length);
    hash.update(bytes);
  };
  field(release);
  for (const item of [...artifacts].sort(compareNames)) {
    field(item.name);
    field(item.content);
  }
  return `sha256:${hash.digest('hex')}`;
}

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
  if (!/^[A-Za-z0-9_-]{1,64}$/.test(projectID) || !release || release.trim() !== release || !memberKey) {
    throw new Error('project ID, release, and member key are required');
  }
  const root = resolve(mapsDir);
  const files = await mapFiles(root);
  if (files.length === 0) throw new Error('no JavaScript source maps found');
  if (files.length > maxBundleMaps) throw new Error('source-map bundle exceeds 512 maps');
  const artifacts = [];
  const seen = new Set();
  for (const file of files) {
    const size = (await stat(file)).size;
    if (size > maxMapBytes) throw new Error('source map exceeds 30 MiB artifact limit');
    const rel = relative(root, file).split(sep).join('/');
    const scriptURL = new URL(rel.slice(0, -4), assets);
    const name = sourceMapNameForURL(scriptURL.href);
    if (seen.has(name)) throw new Error('two source maps resolve to the same script asset name');
    seen.add(name);
    const raw = await readFile(file);
    const content = raw.toString('utf8');
    if (!Buffer.from(content, 'utf8').equals(raw) || content.includes('\0')) {
      throw new Error('source map is not safe UTF-8 JSON text');
    }
    artifacts.push({ name, content });
  }
  artifacts.sort(compareNames);
  const digest = sourceMapBundleDigest(release, artifacts);
  const body = JSON.stringify({ release, digest, artifacts });
  if (Buffer.byteLength(body, 'utf8') > maxBundleBody) throw new Error('source-map bundle exceeds 64 MiB API body limit');
  const endpoint = new URL(`/api/projects/${encodeURIComponent(projectID)}/artifact-bundles`, collector);
  const headers = { Authorization: `Bearer ${memberKey}`, 'Content-Type': 'application/json' };
  const response = await fetcher(endpoint, { method: 'POST', headers, body });
  if (response.status !== 201 && response.status !== 200) {
    throw new Error(`source-map bundle upload failed (HTTP ${response.status})`);
  }
  const accepted = await response.json();
  const checkReceipt = (receipt) => {
    if (!receipt.id || receipt.project_id !== projectID || receipt.release !== release || receipt.digest !== digest ||
        receipt.artifact_count !== artifacts.length || !Array.isArray(receipt.artifacts) ||
        receipt.artifacts.length !== artifacts.length) {
      throw new Error('source-map bundle did not return matching acceptance metadata');
    }
    const expectedBytes = artifacts.reduce((sum, item) => sum + Buffer.byteLength(item.content, 'utf8'), 0);
    if (receipt.total_bytes !== expectedBytes) throw new Error('source-map bundle stored byte count mismatch');
    for (let i = 0; i < artifacts.length; i++) {
      const wanted = artifacts[i];
      const got = receipt.artifacts[i];
      const hash = createHash('sha256').update(wanted.content, 'utf8').digest('hex');
      if (!got?.id || got.name !== wanted.name || got.release !== release ||
          got.parse_status !== 'valid' || got.size !== Buffer.byteLength(wanted.content, 'utf8') ||
          got.content_sha256 !== hash) {
        throw new Error('source-map bundle did not return valid matching artifact receipts');
      }
    }
  };
  checkReceipt(accepted);
  const readback = await fetcher(new URL(`${endpoint.pathname}/${encodeURIComponent(accepted.id)}`, collector), {
    method: 'GET', headers
  });
  if (readback.status !== 200) throw new Error(`source-map bundle readback failed (HTTP ${readback.status})`);
  const durable = await readback.json();
  checkReceipt(durable);
  if (durable.id !== accepted.id || durable.created_at !== accepted.created_at ||
      durable.artifacts.some((item, i) => item.id !== accepted.artifacts[i].id || item.created_at !== accepted.artifacts[i].created_at)) {
    throw new Error('source-map bundle readback changed its accepted identity');
  }
  return durable.artifacts.map((item) => ({ id: item.id, name: item.name, release: item.release }));
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
