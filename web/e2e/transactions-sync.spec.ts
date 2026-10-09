import { expect, test } from '@playwright/test';

// Refresh and filters only read durable history; they never ask a bank to rescan.
test('restores stored transaction history without reviving a legacy sync job', async ({ page }) => {
  const legacyCalls: string[] = [];
  page.on('request', request => {
    if (/\/api\/v1\/(?:connection|monitor|poll-runs)|ensure-history|history-sync-jobs/.test(request.url())) legacyCalls.push(request.url());
  });
  await page.addInitScript(() => localStorage.setItem('acb_active_history_sync_job_id', 'legacy-job'));
  await page.goto('/transactions');
  await expect(page.getByRole('heading', { name: 'Giao dịch', exact: true })).toBeVisible();
  const response = page.waitForResponse(r => /\/api\/v1\/transactions\?/.test(r.url()) && r.request().method() === 'GET');
  await page.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' }).click();
  expect((await response).status()).toBe(200);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Giao dịch', exact: true })).toBeVisible();
  expect(legacyCalls).toEqual([]);
});

test('date and search filters query durable history with no bank synchronization', async ({ page }) => {
  await page.goto('/transactions');
  await page.getByRole('button', { name: 'Tùy chọn', exact: true }).click();
  const inputs = page.locator('input[type=date]');
  await inputs.nth(0).fill('2026-10-01');
  const dateResponse = page.waitForResponse(r => r.url().includes('from=2026-10-01') && r.url().includes('to=2026-10-08'));
  await inputs.nth(1).fill('2026-10-08');
  expect((await dateResponse).status()).toBe(200);
  const searchResponse = page.waitForResponse(r => r.url().includes('q=DH'));
  await page.getByPlaceholder('Tìm theo nội dung chuyển khoản, số tiền, mã giao dịch...').fill('DH');
  expect((await searchResponse).status()).toBe(200);
  await expect(page.getByRole('button', { name: 'Đồng bộ từ ACB' })).toHaveCount(0);
});
