import { expect, test, type Page, type Response } from '@playwright/test';

const cashier = (page: Page) => page.getByRole('region', { name: 'Thu ngân', exact: true });
const historyPanel = (page: Page) => page.getByRole('tabpanel', { name: 'Lịch sử giao dịch', exact: true });
const isHistoryResponse = (response: Response) => {
  const url = new URL(response.url());
  return url.pathname === '/api/v1/transactions' && url.searchParams.get('limit') === '20' &&
    response.request().method() === 'GET';
};

async function selectHistory(page: Page) {
  const tab = page.getByRole('tab', { name: /Lịch sử giao dịch/ });
  await tab.click();
  await expect(tab).toHaveAttribute('aria-selected', 'true');
  await expect(historyPanel(page)).toBeVisible();
}

async function selectCashier(page: Page) {
  const tab = page.getByRole('tab', { name: /Quầy thu ngân/ });
  await tab.click();
  await expect(tab).toHaveAttribute('aria-selected', 'true');
  await expect(cashier(page)).toBeVisible();
}

async function refreshHistory(page: Page) {
  const refresh = historyPanel(page).getByRole('button', { name: 'Tải lại dữ liệu đã lưu' });
  await expect(refresh).toBeEnabled();
  const response = page.waitForResponse(isHistoryResponse);
  await refresh.click();
  expect((await response).status()).toBe(200);
  await expect(refresh).toBeEnabled();
}

test.describe('Browser Full Regression Suite (Desktop/Mobile, State Sharing, Multitab, Roles)', () => {
  test.afterEach(async ({ page }) => {
    await page.unrouteAll({ behavior: 'wait' });
  });

  test('cross-route navigation preserves selected cashier and history surfaces', async ({ page }) => {
    // 1. Visit Overview
    await page.goto('/');
    await expect(page.getByRole('heading', { name: /Tổng quan/ })).toBeVisible();

    // 2. Navigate to Bank Connection
    await page.getByRole('button', { name: 'Kết nối ngân hàng' }).first().click();
    await expect(page).toHaveURL(/\/admin\/connection$/);

    // 3. Navigate to Notifications
    await page.getByRole('button', { name: /Kênh thông báo|Webhooks/ }).first().click();
    await expect(page.getByRole('heading', { name: 'Kênh thông báo', exact: true })).toBeVisible();

    // The cashier and durable history are separate, state-preserving tabs.
    await page.goto('/transactions');
    await expect(page).toHaveURL(/\/transactions/);
    await expect(cashier(page)).toBeVisible();
    await selectHistory(page);
    await expect(page).toHaveURL(/#transactions-history$/);
    await refreshHistory(page);
    await page.reload();
    await expect(historyPanel(page)).toBeVisible();
    await expect(page.getByRole('tab', { name: /Lịch sử giao dịch/ })).toHaveAttribute('aria-selected', 'true');
    await selectCashier(page);
    await expect(page).toHaveURL(/#counter-checkout$/);
  });

  test('multitab device preferences, WiFi and cashier drafts survive refresh and reload', async ({ context }) => {
    await context.addInitScript(() => {
      if (!localStorage.getItem('acb.voice.settings.v1')) {
        localStorage.setItem('acb.voice.settings.v1', JSON.stringify({
          schemaVersion: 2, enabled: true, volume: 0.35, announceWhenHidden: true,
        }));
      }
    });
    const page1 = await context.newPage();
    const page2 = await context.newPage();
    await page1.goto('/');
    await expect(page1.getByRole('heading', { name: /Tổng quan/ })).toBeVisible();
    await page2.goto('/transactions');
    await cashier(page2).getByRole('button', { name: /Chế độ payOS/ }).click();
    const amount = cashier(page2).getByLabel('Số tiền · nghìn đồng');
    await amount.fill('120');

    // Only device-local preferences are shared here, never server voice overrides.
    await page1.getByRole('button', { name: 'Tùy chỉnh giọng đọc' }).click();
    const localVolume1 = page1.locator('input[type="range"][min="0"][max="1"]');
    await localVolume1.press('Home');
    for (let step = 0; step < 5; step++) await localVolume1.press('ArrowRight');
    await expect(localVolume1).toHaveValue('0.25');
    await page1.getByRole('checkbox', { name: /Đọc cả khi chuyển tab khác/ }).uncheck();
    await page2.getByRole('button', { name: 'Tùy chỉnh giọng đọc' }).click();
    const localVolume2 = page2.locator('input[type="range"][min="0"][max="1"]');
    await expect(localVolume2).toHaveValue('0.25');
    await expect(page2.getByRole('checkbox', { name: /Đọc cả khi chuyển tab khác/ })).not.toBeChecked();
    await expect(page2.getByRole('switch', { name: 'Bật đọc giao dịch' })).toHaveAttribute('aria-checked', 'true');
    await page1.getByRole('button', { name: 'Đóng', exact: true }).click();
    await page2.getByRole('button', { name: 'Đóng', exact: true }).click();

    await cashier(page2).getByRole('button', { name: 'WiFi quán', exact: true }).click();
    const wifi2 = page2.getByRole('dialog', { name: 'WiFi quán', exact: true });
    await wifi2.getByRole('button', { name: 'Sửa thông tin WiFi', exact: true }).click();
    await wifi2.getByLabel('Tên WiFi (SSID)', { exact: true }).fill('Browser Regression WiFi');
    await wifi2.getByLabel('Mật khẩu WiFi', { exact: true }).fill('browser-fixture-password');
    await wifi2.getByRole('button', { name: 'Lưu WiFi', exact: true }).click();
    await expect(wifi2.getByRole('img', { name: 'Mã QR kết nối WiFi', exact: true })).toHaveAttribute('src', /^data:image\/png/);
    await wifi2.getByRole('button', { name: 'Đóng', exact: true }).click();

    await page1.goto('/transactions');
    await cashier(page1).getByRole('button', { name: 'WiFi quán', exact: true }).click();
    const wifi1 = page1.getByRole('dialog', { name: 'WiFi quán', exact: true });
    await expect(wifi1.getByText('Browser Regression WiFi', { exact: true })).toBeVisible();
    await wifi1.getByRole('button', { name: 'Đóng', exact: true }).click();
    await selectHistory(page2);
    await refreshHistory(page2);
    await selectCashier(page2);
    await expect(amount).toHaveValue('120');

    await page2.reload();
    await expect(cashier(page2)).toBeVisible();
    await expect(cashier(page2).getByRole('button', { name: /Chế độ payOS/ })).toHaveAttribute('aria-pressed', 'true');
    await expect(amount).toHaveValue('120');
    await page2.getByRole('button', { name: 'Tùy chỉnh giọng đọc' }).click();
    await expect(localVolume2).toHaveValue('0.25');
    await expect(page2.getByRole('checkbox', { name: /Đọc cả khi chuyển tab khác/ })).not.toBeChecked();
    expect(await page2.evaluate(() => JSON.parse(localStorage.getItem('acb.voice.settings.v1')!))).toEqual({
      schemaVersion: 2, enabled: true, volume: 0.25, announceWhenHidden: false,
    });
    await page2.getByRole('button', { name: 'Đóng', exact: true }).click();
    await cashier(page2).getByRole('button', { name: 'WiFi quán', exact: true }).click();
    await expect(wifi2.getByText('Browser Regression WiFi', { exact: true })).toBeVisible();
    await wifi2.getByRole('button', { name: 'Đóng', exact: true }).click();

    await page1.close();
    await selectHistory(page2);
    await refreshHistory(page2);
    await selectCashier(page2);
    await expect(amount).toHaveValue('120');
    await page2.close();
  });

  test('viewer route handles empty transaction state cleanly', async ({ page }) => {
    await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          items: [],
          summary: { count: 0, incoming: 0, outgoing: 0 },
        }),
      });
    });

    await page.goto('/transactions');
    await expect(cashier(page)).toBeVisible();
    await selectHistory(page);
    await expect(historyPanel(page).getByText('Không tìm thấy giao dịch nào')).toBeVisible();
    await refreshHistory(page);
    await expect(historyPanel(page).getByText('Không tìm thấy giao dịch nào')).toBeVisible();
  });

  test('viewer route handles backend error state with retry and feedback', async ({ page }) => {
    let attempts = 0;
    let recover = false;
    await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
      const isHistory = new URL(route.request().url()).searchParams.get('limit') === '20';
      if (isHistory) attempts++;
      if (isHistory && !recover) {
        await route.fulfill({
          status: 500,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'Database busy timeout' }),
        });
      } else {
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            items: [],
            summary: { count: 0, incoming: 0, outgoing: 0 },
          }),
        });
      }
    });

    await page.goto('/transactions');
    await expect(cashier(page)).toBeVisible();
    await cashier(page).getByRole('button', { name: /Chế độ payOS/ }).click();
    const draft = cashier(page).getByLabel('Số tiền · nghìn đồng');
    await draft.fill('120');
    await selectHistory(page);
    const history = historyPanel(page);
    await expect(history.getByText(/Không thể tải dữ liệu:/)).toBeVisible();
    const retry = history.getByRole('button', { name: 'Thử lại', exact: true });
    await expect(retry).toBeVisible();
    const failedAttempts = attempts;
    recover = true;
    const response = page.waitForResponse(isHistoryResponse);
    await retry.click();
    expect((await response).status()).toBe(200);
    await expect(history.getByText('Không tìm thấy giao dịch nào')).toBeVisible();
    await expect(history.getByText(/Không thể tải dữ liệu:/)).toHaveCount(0);
    expect(attempts).toBeGreaterThan(failedAttempts);
    await selectCashier(page);
    await expect(draft).toHaveValue('120');
  });

  test('role isolation: viewer layout vs admin layout', async ({ page }) => {
    await page.goto('/transactions');
    await expect(page.getByRole('region', { name: 'Thu ngân', exact: true })).toBeVisible();
    const bounds = await page.evaluate(() => ({
      viewport: window.innerWidth,
      page: document.documentElement.scrollWidth,
      header: document.querySelector('header')!.getBoundingClientRect().height,
    }));
    expect(bounds.page).toBeLessThanOrEqual(bounds.viewport);
    expect(bounds.header).toBeLessThanOrEqual(64);

    // 2. Direct navigation to unknown route redirects cleanly to root /
    await page.goto('/some-nonexistent-path');
    await expect(page).toHaveURL(/\/(admin)?$/);
  });

  test('public root is the cashier and never requests private administration data', async ({ page, baseURL }) => {
    const apiRequests: string[] = [];
    // Keep role routing independent of cancellation of in-flight backend requests
    // during the forbidden-route redirect; use actual loopback public snapshots.
    const snapshots = new Map<string, unknown>();
    for (const endpoint of ['payment-config', 'sepay-store', 'transactions']) {
      const path = `/api/public/v1/${endpoint}`;
      const response = await page.request.get(`${baseURL ?? 'http://127.0.0.1:5173'}${path}`);
      expect(response.ok()).toBe(true);
      snapshots.set(path, await response.json());
    }
    await page.route('https://transactions.tuannguyenviet.site/**', async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith('/events')) return route.abort();
      if (url.pathname.startsWith('/api/')) {
        apiRequests.push(url.pathname);
        return route.fulfill({ status: snapshots.has(url.pathname) ? 200 : 403, json: snapshots.get(url.pathname) ?? { error: 'Forbidden' } });
      }
      const response = await route.fetch({ url: `${baseURL ?? 'http://127.0.0.1:5173'}${url.pathname}${url.search}` });
      await route.fulfill({ response });
    });
    await page.goto('https://transactions.tuannguyenviet.site/');
    const cashier = page.getByRole('region', { name: 'Thu ngân', exact: true });
    await expect(cashier).toBeVisible();
    await expect(page.getByRole('button', { name: 'Tiền ra', exact: true })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Hủy đơn trên payOS', exact: true })).toHaveCount(0);
    await expect.poll(() => apiRequests.some((path) => path === '/api/public/v1/transactions')).toBe(true);
    expect(apiRequests.filter((path) => path.startsWith('/api/v1/'))).toEqual([]);
    await page.waitForLoadState('networkidle');
    await page.evaluate(() => {
      window.history.pushState(null, '', '/admin/system');
      window.dispatchEvent(new PopStateEvent('popstate'));
    });
    await expect(page).toHaveURL('https://transactions.tuannguyenviet.site/');
    await expect(cashier).toBeVisible();
    expect(apiRequests.filter((path) => path.startsWith('/api/v1/'))).toEqual([]);
    await page.waitForLoadState('networkidle');
  });

});
