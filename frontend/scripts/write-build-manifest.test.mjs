import { afterEach, expect, test } from 'bun:test';
import { createHash } from 'node:crypto';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { browserDsnBinding, writeBuildManifest } from './write-build-manifest.mjs';

const roots = [];
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
  const js = 'throw new Error("test")';
  const map = '{"version":3,"sources":["src/app.ts"],"names":[],"mappings":"AAAA"}';
  await writeFile(join(publicDir, asset), js);
  await writeFile(join(mapsDir, `${asset}.map`), map);
  const output = join(root, 'build-manifest.json');
  const dsn = 'https://publicIngestKey@flare.example/123456789012';
  const manifest = await writeBuildManifest(publicDir, mapsDir, output, dsn);
  expect(manifest.public_files[asset]).toBe(createHash('sha256').update(js).digest('hex'));
  expect(manifest.source_maps[`${asset}.map`]).toBe(createHash('sha256').update(map).digest('hex'));
  expect(manifest.browser_dsn_binding_sha256).toBe(browserDsnBinding(dsn));
  expect(manifest.browser_dsn_binding_sha256).toBe(browserDsnBinding('https://publicIngestKey@other.example/123456789012'));
  expect(manifest.browser_dsn_binding_sha256).not.toBe(browserDsnBinding('https://otherKey@flare.example/123456789012'));
  expect(JSON.stringify(manifest)).not.toContain('publicIngestKey');
  expect(JSON.parse(await readFile(output, 'utf8'))).toEqual(manifest);
  expect(await readFile(join(publicDir, asset), 'utf8')).toBe(js);
});

test('missing DSN does not prevent a build but cannot satisfy the upload gate', () => {
  expect(browserDsnBinding('')).toBe('');
  expect(() => browserDsnBinding('https://key:password@flare.example/123456789012')).toThrow('invalid browser DSN build input');
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
