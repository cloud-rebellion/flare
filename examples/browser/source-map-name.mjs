// Name one release source-map artifact for a script URL as the browser bridge
// does. Use the exact public URL loaded by that release (including its host),
// then upload the matching .map content under the printed name.
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';

const compiledScriptPaths = [
  /^\/_app\/immutable\/(?:chunks|entry|nodes)\/(?:[A-Za-z0-9_-]{1,64}\.)?[A-Za-z0-9_-]{8,80}\.m?js$/,
  /^\/_astro\/(?:[A-Za-z0-9_-]{1,64}\.)?[A-Za-z0-9_-]{8,80}\.m?js$/,
  /^\/(?:assets|build\/assets|static\/js)\/[A-Za-z0-9._-]{1,64}[-.][A-Za-z0-9_-]{8,80}\.m?js$/
];

export function sourceMapNameForURL(raw) {
  const url = new URL(raw);
  if (!['http:', 'https:'].includes(url.protocol) || !compiledScriptPaths.some((pattern) => pattern.test(url.pathname))) {
    throw new Error('expected a compiled JavaScript asset URL');
  }
  return `flare-asset-sha256:${createHash('sha256').update(url.origin + url.pathname).digest('hex')}`;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  try {
    if (process.argv.length !== 3) throw new Error('usage: bun source-map-name.mjs <public-script-url>');
    console.log(sourceMapNameForURL(process.argv[2]));
  } catch (error) {
    // Do not echo the URL: a mistaken token-bearing URL must not enter CI logs.
    console.error(error instanceof Error ? error.message : 'invalid script URL');
    process.exitCode = 1;
  }
}
