import { afterEach, expect, test } from 'bun:test';
import { createHash } from 'node:crypto';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { browserDsnBinding, browserDsnOriginBinding, writeBuildManifest } from './write-build-manifest.mjs';

const roots = [];
const sourceCommit = 'a'.repeat(40);
const release = `v0.4.0+g${sourceCommit}`;
afterEach(async () => {
  for (const root of roots.splice(0)) await rm(root, { recursive: true, force: true });
});

test('the runtime manifest binds exact public bytes to their private map bytes', async () => {
  const root = await mkdtemp(join(tmpdir(), 'flare-manifest-'));
  roots.push(root);
  const publicDir = join(root, 'build');
  const mapsDir = join(root, 'source-maps');
  await mkdir(join(publicDir, '_app/immutable/chunks'), { recursive: true });
  await mkdir(join(mapsDir, '_app/immutable/chunks'), { recursive: true });
  const asset = '_app/immutable/chunks/app.12345678.js';
  const dsn = 'https://publicIngestKey@flare.example/123456789012';
  const js = `const dsn = ${JSON.stringify(dsn)}; const release = ${JSON.stringify(release)}; throw new Error("test")`;
  const map = '{"version":3,"sources":["src/app.ts"],"names":[],"mappings":"AAAA"}';
  await writeFile(join(publicDir, asset), js);
  await writeFile(join(mapsDir, `${asset}.map`), map);
  const output = join(root, 'build-manifest.json');
  const manifest = await writeBuildManifest(publicDir, mapsDir, output, dsn, release);
  expect(manifest.public_files[asset]).toBe(createHash('sha256').update(js).digest('hex'));
  expect(manifest.source_maps[`${asset}.map`]).toBe(createHash('sha256').update(map).digest('hex'));
  expect(manifest.browser_dsn_binding_sha256).toBe(browserDsnBinding(dsn));
  expect(manifest.browser_dsn_binding_sha256).toBe('cc35dd84cc57957b4ee2db717e53805ce4b6834f654f36fdb28db1c993b8d234');
  expect(manifest.browser_dsn_binding_sha256).toBe(browserDsnBinding('https://publicIngestKey@other.example/123456789012'));
  expect(manifest.browser_dsn_origin_sha256).toBe(browserDsnOriginBinding(dsn));
  expect(manifest.browser_dsn_origin_sha256).toBe(createHash('sha256').update('flare-browser-dsn-origin-v1\nhttps://flare.example').digest('hex'));
  expect(manifest.browser_dsn_origin_sha256).not.toBe(browserDsnOriginBinding('https://publicIngestKey@other.example/123456789012'));
  expect(manifest.browser_dsn_binding_sha256).not.toBe(browserDsnBinding('https://otherKey@flare.example/123456789012'));
  expect(JSON.stringify(manifest)).not.toContain('publicIngestKey');
  expect(JSON.parse(await readFile(output, 'utf8'))).toEqual(manifest);
  expect(await readFile(join(publicDir, asset), 'utf8')).toBe(js);
});

test('missing DSN does not prevent a build but cannot satisfy the upload gate', () => {
  expect(browserDsnBinding('')).toBe('');
  expect(browserDsnOriginBinding('')).toBe('');
  expect(() => browserDsnBinding('https://key:password@flare.example/123456789012')).toThrow('invalid browser DSN build input');
  expect(() => browserDsnBinding('https://key@flare.example/wrong/123456789012')).toThrow('invalid browser DSN build input');
});

test('a configured DSN requires its exact value and full-commit release in compiled JavaScript', async () => {
  const root = await mkdtemp(join(tmpdir(), 'flare-manifest-'));
  roots.push(root);
  const publicDir = join(root, 'build');
  const mapsDir = join(root, 'source-maps');
  const output = join(root, 'build-manifest.json');
  const dsn = 'https://publicIngestKey@flare.example/123456789012';
  await mkdir(publicDir);
  await mkdir(mapsDir);
  await writeFile(join(mapsDir, 'app.js.map'), '{"version":3}');
  await writeFile(join(publicDir, 'app.js'), `const dsn = ${JSON.stringify(dsn)};`);
  await expect(writeBuildManifest(publicDir, mapsDir, output, dsn, 'v0.4.0'))
    .rejects.toThrow('full source commit');
  await expect(writeBuildManifest(publicDir, mapsDir, output, dsn, release))
    .rejects.toThrow('missing from one compiled JavaScript asset');
  await writeFile(join(publicDir, 'app.js'), `const release = ${JSON.stringify(release)};`);
  await expect(writeBuildManifest(publicDir, mapsDir, output, dsn, release))
    .rejects.toThrow('missing from one compiled JavaScript asset');
  await writeFile(join(publicDir, 'app.js'), `const dsn = ${JSON.stringify(dsn)}; const release = ${JSON.stringify(release)};`);
  await expect(writeBuildManifest(publicDir, mapsDir, output, dsn, release)).resolves.toMatchObject({
    browser_dsn_origin_sha256: browserDsnOriginBinding(dsn)
  });
  const unconfigured = await writeBuildManifest(publicDir, mapsDir, output, '', '');
  expect(unconfigured.browser_dsn_binding_sha256).toBe('');
  expect(unconfigured.browser_dsn_origin_sha256).toBe('');
});

test('a private map without the deployed asset cannot receive a manifest', async () => {
  const root = await mkdtemp(join(tmpdir(), 'flare-manifest-'));
  roots.push(root);
  const publicDir = join(root, 'build');
  const mapsDir = join(root, 'source-maps');
  await mkdir(publicDir);
  await mkdir(mapsDir);
  await writeFile(join(publicDir, 'app.js'), 'a');
  await writeFile(join(mapsDir, 'other.js.map'), 'b');
  await expect(writeBuildManifest(publicDir, mapsDir, join(root, 'manifest.json')))
    .rejects.toThrow('each source map must match a public file');
});
