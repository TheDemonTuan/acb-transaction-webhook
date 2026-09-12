import { expect, test } from '@playwright/test';

test('distinguishes cached refresh from ACB synchronization and shows sync errors', async ({ page }) => {
  let transactionReads = 0;
  let syncCalls = 0;

  await page.route('**/api/v1/transactions/ensure-history', async (route) => {
    syncCalls += 1;
    await route.fulfill({
      status: 502,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'Phiên ACB đã hết hạn. Vui lòng đăng nhập lại.' }),
    });
  });
  await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
    transactionReads += 1;
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [], summary: { count: 0, incoming: 0, outgoing: 0 } }),
    });
  });

  await page.goto('/transactions');
  const refresh = page.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' });
  const sync = page.getByRole('button', { name: 'Đồng bộ từ ACB' });
  await expect(refresh).toBeVisible();
  await expect(sync).toBeVisible();
  await expect(sync).toBeDisabled();

  const initialReads = transactionReads;
  await refresh.click();
  await expect.poll(() => transactionReads).toBeGreaterThan(initialReads);
  expect(syncCalls).toBe(0);

  await page.getByRole('button', { name: 'Hôm nay' }).click();
  await expect(sync).toBeEnabled();
  await sync.click();
  await expect(page.getByRole('alert')).toContainText('Phiên ACB đã hết hạn. Vui lòng đăng nhập lại.');
  expect(syncCalls).toBe(1);
});
