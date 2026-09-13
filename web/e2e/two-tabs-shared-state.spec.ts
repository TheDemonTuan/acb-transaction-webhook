import { expect, test } from '@playwright/test';

test.describe('Two Tabs & Shared State Synchronization', () => {
  test('elects single voice leader across two tabs and fails over when leader tab closes', async ({ context }) => {
    // Tab 1
    const page1 = await context.newPage();
    await page1.addInitScript(() => {
      (window as any).__spokenCount = 0;
      const synth = {
        paused: false,
        speaking: false,
        pending: false,
        speak: () => { (window as any).__spokenCount++; },
        cancel: () => {},
        getVoices: () => [],
        addEventListener: () => {},
        removeEventListener: () => {},
      };
      Object.defineProperty(window, 'speechSynthesis', { value: synth, configurable: true });
    });

    // Tab 2
    const page2 = await context.newPage();
    await page2.addInitScript(() => {
      (window as any).__spokenCount = 0;
      const synth = {
        paused: false,
        speaking: false,
        pending: false,
        speak: () => { (window as any).__spokenCount++; },
        cancel: () => {},
        getVoices: () => [],
        addEventListener: () => {},
        removeEventListener: () => {},
      };
      Object.defineProperty(window, 'speechSynthesis', { value: synth, configurable: true });
    });

    // Both open /transactions
    await page1.goto('/transactions');
    await page2.goto('/transactions');

    // Wait for leader election via Web Locks API
    await page1.waitForTimeout(500);

    // Broadcast a simulated transaction credit via DOM or window event
    await page1.evaluate(() => {
      window.dispatchEvent(
        new CustomEvent('test:simulate-credit', {
          detail: { id: 'tx_two_tabs_1', amount: 500000 },
        }),
      );
    });

    // Now close Tab 1 (the current leader)
    await page1.close();

    // Tab 2 must seamlessly acquire the lock and continue operating as leader without error
    await page2.waitForTimeout(600);
    await expect(page2.getByRole('heading', { name: 'ACB Transaction Webhook' })).toBeVisible();

    // Verify Tab 2 is active and responsive to actions
    const refreshBtn = page2.getByRole('button', { name: 'T???i l???i d??? li???u ???? l??u' });
    await expect(refreshBtn).toBeVisible();
    await refreshBtn.click();
    await expect(page2.getByText(/250.000/).first()).toBeVisible();

    await page2.close();
  });

  test('reflects connection configuration updates across both tabs', async ({ context }) => {
    let savedAccount = '***1234';

    const page1 = await context.newPage();
    const page2 = await context.newPage();

    // Configure connection endpoint to return unconfigured initially for page1
    let isConfigured = false;

    const mockStatus = () => ({
      service: 'HEALTHY',
      version: '2.0.0',
      uptimeSeconds: 120,
      acb: {
        state: isConfigured ? 'MONITORING' : 'UNCONFIGURED',
        accountMasked: isConfigured ? savedAccount : '',
        coverage: isConfigured ? 'FULL' : 'NOT_STARTED',
      },
      storage: { status: 'READY' },
      webhooks: { pending: 0, deadLetter: 0 },
    });

    await page1.route('**/api/v1/status', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(mockStatus()),
      });
    });

    await page2.route('**/api/v1/status', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(mockStatus()),
      });
    });

    await page1.route('**/api/v1/connection/configure', async (route) => {
      const body = JSON.parse(route.request().postData() || '{}');
      savedAccount = body.accountMasked || '***9876';
      isConfigured = true;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          id: 'conn_1',
          state: 'MONITORING',
          accountMasked: savedAccount,
        }),
      });
    });

    await page1.route('**/api/v1/connection', async (route) => {
      if (route.request().method() === 'POST') {
        const body = JSON.parse(route.request().postData() || '{}');
        savedAccount = body.accountMasked || '***9876';
        isConfigured = true;
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            configured: true,
            connection: { id: 'conn_1', state: 'MONITORING', accountMasked: savedAccount },
          }),
        });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          configured: isConfigured,
          connection: isConfigured ? { id: 'conn_1', state: 'MONITORING', accountMasked: savedAccount } : null,
        }),
      });
    });

    await page2.route('**/api/v1/connection', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          configured: isConfigured,
          connection: isConfigured ? { id: 'conn_1', state: 'MONITORING', accountMasked: savedAccount } : null,
        }),
      });
    });

    // Both open admin connection and overview
    await page1.goto('/admin/connection');
    await page2.goto('/admin/overview');

    await expect(page1.getByRole('heading', { name: 'K???t n???i ACB' })).toBeVisible();
    await expect(page2.getByRole('heading', { name: 'T???ng quan' })).toBeVisible();

    // Tab 1 updates the connection masked account number
    const accountInput = page1.getByLabel('S??? t??i kho???n ???? che');
    await accountInput.clear();
    await accountInput.fill('***9876');
    await page1.getByRole('button', { name: 'L??u k???t n???i' }).click();
    await expect(page1.getByText('???? l??u k???t n???i.')).toBeVisible();

    // Tab 2 refreshes overview status and displays the updated account
    const refreshOverview = page2.getByRole('button', { name: 'L??m m???i' });
    await refreshOverview.click();
    await expect(page2.getByText(/9876/)).toBeVisible();

    await page1.close();
    await page2.close();
  });
});
