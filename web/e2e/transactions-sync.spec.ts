import { expect, test, type Page, type Response } from '@playwright/test';

const historyPanel = (page: Page) => page.getByRole('tabpanel', { name: 'Lịch sử giao dịch', exact: true });
const isHistoryResponse = (response: Response) => {
  const url = new URL(response.url());
  return url.pathname === '/api/v1/transactions' && url.searchParams.get('limit') === '20' &&
    response.request().method() === 'GET';
};

async function openHistory(page: Page) {
  await page.goto('/transactions');
  const historyTab = page.getByRole('tab', { name: /Lịch sử giao dịch/ });
  await historyTab.click();
  await expect(historyTab).toHaveAttribute('aria-selected', 'true');
  await expect(historyPanel(page)).toBeVisible();
}

function trackLegacyCalls(page: Page) {
  const calls: string[] = [];
  page.on('request', request => {
    if (/\/api\/v1\/(?:connection|monitor|poll-runs)|ensure-history|history-sync-jobs/.test(request.url())) calls.push(request.url());
  });
  return calls;
}

// Refresh and filters only read durable history; they never ask a bank to rescan.
test('restores stored transaction history without reviving a legacy sync job', async ({ page }) => {
  const legacyCalls = trackLegacyCalls(page);
  await page.addInitScript(() => localStorage.setItem('acb_active_history_sync_job_id', 'legacy-job'));
  await openHistory(page);
  const refresh = historyPanel(page).getByRole('button', { name: 'Tải lại dữ liệu đã lưu' });
  await expect(refresh).toBeEnabled();
  const response = page.waitForResponse(isHistoryResponse);
  await refresh.click();
  const refreshed = await response;
  expect(refreshed.status()).toBe(200);
  const storedHistory = await refreshed.json();
  await expect(refresh).toBeEnabled();

  const reloadResponse = page.waitForResponse(isHistoryResponse);
  await page.reload();
  const restored = await reloadResponse;
  expect(restored.status()).toBe(200);
  expect((await restored.json()).items).toEqual(storedHistory.items);
  await expect(historyPanel(page)).toBeVisible();
  await expect(refresh).toBeEnabled();
  expect(legacyCalls).toEqual([]);
});

test('date and search filters query durable history with no bank synchronization', async ({ page }) => {
  const legacyCalls = trackLegacyCalls(page);
  await openHistory(page);
  const history = historyPanel(page);
  await history.getByRole('button', { name: 'Tùy chọn', exact: true }).click();
  const inputs = history.locator('input[type=date]');
  await inputs.nth(0).fill('2026-10-01');
  const dateResponse = page.waitForResponse(response => {
    const params = new URL(response.url()).searchParams;
    return isHistoryResponse(response) && params.get('from') === '2026-10-01' && params.get('to') === '2026-10-08';
  });
  await inputs.nth(1).fill('2026-10-08');
  expect((await dateResponse).status()).toBe(200);
  const searchResponse = page.waitForResponse(response => {
    const params = new URL(response.url()).searchParams;
    return isHistoryResponse(response) && params.get('q') === 'DH' &&
      params.get('from') === '2026-10-01' && params.get('to') === '2026-10-08';
  });
  await history.locator('input[type="text"]').fill('DH');
  expect((await searchResponse).status()).toBe(200);
  await expect(history.getByRole('button', { name: 'Tải lại dữ liệu đã lưu' })).toBeEnabled();
  expect(legacyCalls).toEqual([]);
});
