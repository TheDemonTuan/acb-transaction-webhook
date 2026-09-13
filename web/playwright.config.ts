import { defineConfig, devices } from '@playwright/test';

const isWin = process.platform === 'win32';
const webServerCommand = isWin
  ? 'powershell -NoProfile -Command "$ErrorActionPreference=\'SilentlyContinue\'; cd ..; $p = Join-Path $env:TEMP \'tbg-playwright\'; Remove-Item -Recurse -Force $p; $env:DATA_DIR=$p; $env:LISTEN_ADDR=\'127.0.0.1:18081\'; $env:APP_MASTER_KEY=\'0123456789012345678901234567890123456789012345678901234567890123\'; go run ./cmd/gateway"'
  : 'cd .. && rm -rf /tmp/tbg-playwright && DATA_DIR=/tmp/tbg-playwright LISTEN_ADDR=127.0.0.1:18081 APP_MASTER_KEY=0123456789012345678901234567890123456789012345678901234567890123 go run ./cmd/gateway';

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  workers: 1,
  use: {
    baseURL: process.env.E2E_BASE_URL ?? 'http://127.0.0.1:18081',
    browserName: 'chromium',
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['iPhone 13'], browserName: 'chromium' } },
  ],
  webServer: process.env.E2E_SKIP_WEBSERVER
    ? undefined
    : {
        command: webServerCommand,
        url: 'http://127.0.0.1:18081/healthz',
        reuseExistingServer: false,
        timeout: 60_000,
      },
});
