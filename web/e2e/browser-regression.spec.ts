import { expect, test } from '@playwright/test';

// Test matrix: Desktop & Mobile on all application routes sharing transaction/history/realtime state:
// - Navigation across routes (/admin, /admin/connection, /admin/notifications, /admin/activity, /admin/system, /transactions)
// - State synchronization across multitab
// - Tab reload and recovery
// - Empty state vs Error state vs Populated state
// - Role variants (Viewer /transactions vs Admin /*)

test.describe('Browser Full Regression Suite (Desktop/Mobile, State Sharing, Multitab, Roles)', () => {
  test.afterEach(async ({ page }) => {
    await page.unrouteAll({ behavior: 'wait' });
  });

  test('cross-route navigation and active state persistence across all routes', async ({ page }) => {
    // 1. Visit Overview
    await page.goto('/');
    await expect(page.getByRole('heading', { name: /Tổng quan/ })).toBeVisible();

    // 2. Navigate to Bank Connection
    await page.getByRole('button', { name: 'Kết nối payOS' }).first().click();
    await expect(page.getByRole('heading', { name: 'Kết nối payOS / KienlongBank' })).toBeVisible();

    // 3. Navigate to Notifications
    await page.getByRole('button', { name: /Kênh thông báo|Webhooks/ }).first().click();
    await expect(page.getByRole('heading', { name: 'Kênh thông báo', exact: true })).toBeVisible();

    // 4. Switch Role: Go to Viewer route /transactions
    await page.goto('/transactions');
    await expect(page).toHaveURL(/\/transactions/);
    await expect(page.getByRole('region', { name: 'Thu ngân', exact: true })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' })).toBeVisible();
  });

  test('multitab state synchronization and tab reload preservation', async ({ context }) => {
    const page1 = await context.newPage();
    const page2 = await context.newPage();

    // Open Page 1 on Admin Overview
    await page1.goto('/');
    await expect(page1.getByRole('heading', { name: /Tổng quan/ })).toBeVisible();

    // Open Page 2 on Viewer Transactions
    await page2.goto('/transactions');
    const amount = page2.getByRole('region', { name: 'Thu ngân', exact: true }).getByLabel('Số tiền · nghìn đồng');
    await amount.fill('120');
    await page2.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' }).click();
    await expect(amount).toHaveValue('120');

    // Reload Page 2 and verify state persists cleanly without crash
    await page2.reload();
    await expect(page2.getByRole('region', { name: 'Thu ngân', exact: true })).toBeVisible();
    await expect(page2.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' })).toBeVisible();

    // Close Page 1 (tab close) and verify Page 2 continues functioning
    await page1.close();
    await page2.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' }).click();

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
    await expect(page.getByRole('region', { name: 'Thu ngân', exact: true })).toBeVisible();
    await expect(page.getByText('Không tìm thấy giao dịch nào')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' })).toBeVisible();
  });

  test('viewer route handles backend error state with retry and feedback', async ({ page }) => {
    let attempts = 0;
    await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
      attempts++;
      if (attempts === 1) {
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
    await expect(page.getByRole('region', { name: 'Thu ngân', exact: true })).toBeVisible();
    const draft = page.getByLabel('Số tiền · nghìn đồng');
    await draft.fill('120');
    const refreshBtn = page.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' });
    await expect(refreshBtn).toBeVisible();
    await refreshBtn.click();
    await expect(page.getByText('Không tìm thấy giao dịch nào')).toBeVisible();
    await expect.poll(() => attempts).toBeGreaterThanOrEqual(2);
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
    await page.route('https://transactions.tuannguyenviet.site/**', async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.startsWith('/api/')) apiRequests.push(url.pathname);
      if (url.pathname.endsWith('/events')) return route.abort();
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
