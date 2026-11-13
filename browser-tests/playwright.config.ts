import { defineConfig } from '@playwright/test';

const base = new URL(process.env.FLARE_BROWSER_BASE_URL ?? '');
if (base.protocol !== 'http:' || base.hostname !== '127.0.0.1' || !base.port ||
    base.pathname !== '/' || base.search || base.hash || base.username || base.password) {
  throw new Error('The qualification requires its owned loopback HTTP server');
}
if (!process.env.FLARE_BROWSER_REPORT || !process.env.FLARE_BROWSER_OUTPUT) {
  throw new Error('Explicit browser evidence paths are required');
}

export default defineConfig({
  testDir: '.',
  testMatch: 'oidc-portal-launch.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 20_000,
  globalTimeout: 180_000,
  reporter: [['json', { outputFile: process.env.FLARE_BROWSER_REPORT }]],
  outputDir: process.env.FLARE_BROWSER_OUTPUT,
  preserveOutput: 'failures-only',
  use: {
    baseURL: base.origin,
    browserName: 'chromium',
    headless: true,
    viewport: { width: 1280, height: 900 },
    serviceWorkers: 'block',
    acceptDownloads: false,
    trace: 'off',
    video: 'off',
    screenshot: 'only-on-failure',
    launchOptions: {
      ...(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : {}),
      args: ['--no-proxy-server']
    }
  }
});
