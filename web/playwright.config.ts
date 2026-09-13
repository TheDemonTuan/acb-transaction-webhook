import { defineConfig, devices } from '@playwright/test';

const isWin = process.platform === 'win32';
const stagingBaseUrl = process.env.STAGING_BASE_URL;
const targetBaseUrl = stagingBaseUrl || process.env.E2E_BASE_URL || 'http://127.0.0.1:18081';

const gatewayWebServerCommand = isWin
  ? 'powershell -NoProfile -Command "$ErrorActionPreference=\'SilentlyContinue\'; cd ..; $p = Join-Path $env:TEMP \'tbg-playwright\'; Remove-Item -Recurse -Force $p; $env:DATA_DIR=$p; $env:LISTEN_ADDR=\'127.0.0.1:18081\'; $env:APP_MASTER_KEY=\'0123456789012345678901234567890123456789012345678901234567890123\'; go run ./cmd/gateway"'
  : 'cd .. && rm -rf /tmp/tbg-playwright && DATA_DIR=/tmp/tbg-playwright LISTEN_ADDR=127.0.0.1:18081 APP_MASTER_KEY=0123456789012345678901234567890123456789012345678901234567890123 go run ./cmd/gateway';

const fixtureWebServerCommand = 'bun run e2e/fixtures/fixture-server.ts';

const webServerCommand = process.env.E2E_SERVER === 'gateway'
  ? gatewayWebServerCommand
  : fixtureWebServerCommand;

const skipWebServer = Boolean(stagingBaseUrl || process.env.E2E_SKIP_WEBSERVER);

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  workers: 1,
  use: {
    baseURL: targetBaseUrl,
    browserName: 'chromium',
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'] },
      testMatch: '**/*.spec.ts',
    },
    {
      name: 'mobile',
      use: { ...devices['iPhone 13'], browserName: 'chromium' },
      testMatch: /.*(smoke|layout).*\.spec\.ts/,
    },
  ],
  webServer: skipWebServer
    ? undefined
    : {
        command: webServerCommand,
        url: `${targetBaseUrl}/healthz`,
        reuseExistingServer: true,
        timeout: 60_000,
      },
});
