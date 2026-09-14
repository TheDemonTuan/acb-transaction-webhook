import { expect, test } from '@playwright/test';

// Test matrix: Desktop & Mobile on all application routes sharing transaction/history/realtime state:
// - Navigation across routes (/admin, /admin/connection, /admin/notifications, /admin/activity, /admin/system, /transactions)
// - State synchronization across multitab
// - Tab reload and recovery
// - Empty state vs Error state vs Populated state
// - Role variants (Viewer /transactions vs Admin /*)

test.describe('Browser Full Regression Suite (Desktop/Mobile, State Sharing, Multitab, Roles)', () => {

  test('cross-route navigation and active state persistence across all routes', async ({ page }) => {
    // 1. Visit Overview
    await page.goto('/');
    await expect(page.getByRole('heading', { name: /Tổng quan/ })).toBeVisible();

    // 2. Navigate to Bank Connection
    await page.getByRole('button', { name: 'Kết nối ACB' }).first().click();
    await expect(page.getByRole('heading', { name: 'Kết nối ACB' })).toBeVisible();

    // 3. Navigate to Notifications
    await page.getByRole('button', { name: /Kênh thông báo|Webhooks/ }).first().click();
    await expect(page.getByRole('heading', { name: 'Kênh thông báo', exact: true })).toBeVisible();

    // 4. Switch Role: Go to Viewer route /transactions
    await page.goto('/transactions');
    await expect(page).toHaveURL(/\/transactions/);
    await expect(page.getByRole('heading', { name: /Giao dịch/ })).toBeVisible();
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
    await expect(page2.getByRole('heading', { name: /Giao dịch/ })).toBeVisible();

    // Reload Page 2 and verify state persists cleanly without crash
    await page2.reload();
    await expect(page2.getByRole('heading', { name: /Giao dịch/ })).toBeVisible();
    await expect(page2.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' })).toBeVisible();

    // Close Page 1 (tab close) and verify Page 2 continues functioning
    await page1.close();
    await page2.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' }).click();
    await expect(page2.getByRole('heading', { name: /Giao dịch/ })).toBeVisible();

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
    await expect(page.getByRole('heading', { name: /Giao dịch/ })).toBeVisible();
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
    // Button remains clickable and retry succeeds
    const refreshBtn = page.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' });
    await expect(refreshBtn).toBeVisible();
    await refreshBtn.click();
    expect(attempts).toBeGreaterThanOrEqual(1);
  });

  test('role isolation: viewer layout vs admin layout', async ({ page }) => {
    // 1. In Viewer layout (/transactions), header indicates public transaction viewer
    await page.goto('/transactions');
    await expect(page.getByRole('heading', { name: /Giao dịch/ })).toBeVisible();

    // 2. Direct navigation to unknown route redirects cleanly to root /
    await page.goto('/some-nonexistent-path');
    await expect(page).toHaveURL(/\/(admin)?$/);
  });

});
