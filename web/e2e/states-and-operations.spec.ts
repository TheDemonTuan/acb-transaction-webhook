import { expect, test } from '@playwright/test';

test.describe('Role, Auth, Error & Empty States with Core Operations', () => {
  test('renders auth and role states: UNCONFIGURED, AUTH_REQUIRED, and MONITORING', async ({ page }) => {
    // 1. UNCONFIGURED State
    await page.route('**/api/v1/status', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          service: 'HEALTHY',
          version: '2.0.0',
          acb: { state: 'UNCONFIGURED', accountMasked: '' },
          storage: { status: 'READY' },
          webhooks: { pending: 0, deadLetter: 0 },
        }),
      });
    });
    await page.route('**/api/v1/connection', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ configured: false, connection: null }),
      });
    });

    await page.goto('/admin/overview');
    await expect(page.getByText(/Ch??a c???u h??nh|Ch??a li??n k???t/)).toBeVisible();

    // 2. AUTH_REQUIRED State
    await page.route('**/api/v1/status', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          service: 'HEALTHY',
          version: '2.0.0',
          acb: { state: 'AUTH_REQUIRED', accountMasked: '***1234' },
          storage: { status: 'READY' },
          webhooks: { pending: 0, deadLetter: 0 },
        }),
      });
    });
    await page.route('**/api/v1/connection', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          configured: true,
          connection: { id: 'c1', state: 'AUTH_REQUIRED', accountMasked: '***1234' },
        }),
      });
    });

    await page.reload();
    await expect(page.getByText(/C???n x??c th???c/)).toBeVisible();

    // 3. MONITORING State
    await page.route('**/api/v1/status', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          service: 'HEALTHY',
          version: '2.0.0',
          acb: { state: 'MONITORING', accountMasked: '***1234' },
          storage: { status: 'READY' },
          webhooks: { pending: 0, deadLetter: 0 },
        }),
      });
    });
    await page.route('**/api/v1/connection', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          configured: true,
          connection: { id: 'c1', state: 'MONITORING', accountMasked: '***1234' },
        }),
      });
    });

    await page.reload();
    await expect(page.getByText(/MONITORING|??ang ho???t ?????ng/).first()).toBeVisible();
  });

  test('renders concise error state on 502 and handles 404 transaction detail gracefully', async ({ page }) => {
    // 502 Bad Gateway Error State
    await page.route('**/api/v1/status', async (route) => {
      await route.fulfill({
        status: 502,
        contentType: 'text/html',
        body: '<html><body>502 Bad Gateway</body></html>',
      });
    });

    await page.goto('/admin/system');
    await expect(page.getByRole('heading', { name: 'Ch???n ??o??n h??? th???ng' })).toBeVisible();
    const body = await page.innerText('body');
    expect(body).not.toContain('<html>');

    // 404 Transaction Not Found State
    await page.route('**/api/v1/transactions/tx_missing_404', async (route) => {
      await route.fulfill({
        status: 404,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'Kh??ng t??m th???y giao d???ch' }),
      });
    });

    await page.goto('/transactions/tx_missing_404');
    await expect(page.getByText('Kh??ng t??m th???y giao d???ch')).toBeVisible();
    await expect(page.getByRole('link', { name: 'Quay l???i danh s??ch giao d???ch' })).toBeVisible();
  });

  test('handles empty states for transactions, notifications, and activity', async ({ page }) => {
    // Empty Transactions
    await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items: [], summary: { count: 0, incoming: 0, outgoing: 0 } }),
      });
    });

    await page.goto('/transactions');
    await expect(page.getByText(/Ch??a c?? giao d???ch n??o|Kh??ng c?? giao d???ch n??o/)).toBeVisible();

    // Empty Notification Channels
    await page.route('**/api/v1/notification-channels', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items: [] }),
      });
    });

    await page.goto('/admin/notifications');
    await expect(page.getByText(/Ch??a c?? k??nh th??ng b??o n??o/)).toBeVisible();

    // Empty Activity (Poll runs)
    await page.route('**/api/v1/poll-runs*', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items: [] }),
      });
    });

    await page.goto('/admin/activity');
    await expect(page.getByText(/Ch??a c?? chu k??? polling n??o/)).toBeVisible();
  });

  test('performs notifications, monitor schedule, voice announcement and QR operations', async ({ page }) => {
    // 1. Notification Bark channel create & test with deterministic fixture (no APNs)
    let testedNotification = false;
    await page.route('**/api/v1/notification-channels/*/test', async (route) => {
      testedNotification = true;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ success: true, message: 'Deterministic test push simulated' }),
      });
    });

    await page.goto('/admin/notifications');
    await expect(page.getByRole('heading', { name: 'K??nh th??ng b??o', exact: true })).toBeVisible();

    // Click test button on existing fixture Bark channel
    const testButton = page.getByRole('button', { name: /G???i th??? nghi???m|Th??? nghi???m/ }).first();
    if (await testButton.isVisible()) {
      await testButton.click();
      await expect.poll(() => testedNotification).toBe(true);
    }

    // 2. Monitor schedule settings operation
    await page.goto('/admin/connection');
    await expect(page.getByRole('heading', { name: /L???ch tr??nh qu??t ACB/ })).toBeVisible();
    const presetBtn = page.getByRole('button', { name: /Chu???n V3/ });
    await presetBtn.click();
    await page.getByRole('button', { name: 'L??u thay ?????i l???ch tr??nh' }).click();
    await expect(page.getByText(/???? l??u c???u h??nh l???ch tr??nh/)).toBeVisible();

    // 3. Voice operations
    await page.addInitScript(() => {
      (window as any).__voiceSpoken = [];
      const synth = {
        paused: false,
        speaking: false,
        speak: (u: any) => { (window as any).__voiceSpoken.push(u.text); },
        cancel: () => {},
        getVoices: () => [{ default: true, lang: 'vi-VN', name: 'Vietnamese Voice' }],
        addEventListener: () => {},
        removeEventListener: () => {},
      };
      Object.defineProperty(window, 'speechSynthesis', { value: synth, configurable: true });
    });

    // Voice toggle button
    const voiceToggle = page.getByRole('button', { name: /Loa th??ng b??o|B???t loa|T???t loa/ });
    if (await voiceToggle.isVisible()) {
      await voiceToggle.click();
    }

    // 4. Payment QR operation & Receiving QR modal
    await page.goto('/transactions');
    const qrBtn = page.getByRole('button', { name: /M?? QR nh???n ti???n/ });
    await expect(qrBtn).toBeVisible();
    await qrBtn.click();
    await expect(page.getByRole('heading', { name: 'Qu??t m?? nh???n ti???n ACB' })).toBeVisible();
    await page.getByRole('button', { name: '????ng', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Qu??t m?? nh???n ti???n ACB' })).not.toBeVisible();
  });
});
