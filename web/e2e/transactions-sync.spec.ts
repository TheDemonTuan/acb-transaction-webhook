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

test('tracks asynchronous history sync job: accepted -> running -> completed -> transaction refetch', async ({ page }) => {
  let ensureCalls = 0;
  let pollCalls = 0;
  let transactionFetches = 0;

  await page.route('**/api/v1/transactions/ensure-history', async (route) => {
    ensureCalls += 1;
    await route.fulfill({
      status: 202,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'syncjob_e2e_1',
        status: 'QUEUED',
        coverage: 'PENDING',
        synced: false,
        job: {
          id: 'syncjob_e2e_1',
          status: 'QUEUED',
          rangeFrom: '2026-09-14',
          rangeTo: '2026-09-14',
          pagesDone: 0,
          rowsSeen: 0,
          currentDay: '2026-09-14',
        },
      }),
    });
  });

  await page.route('**/api/v1/transactions/history-sync-jobs/syncjob_e2e_1', async (route) => {
    pollCalls += 1;
    if (pollCalls === 1) {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          id: 'syncjob_e2e_1',
          status: 'RUNNING',
          rangeFrom: '2026-09-14',
          rangeTo: '2026-09-14',
          pagesDone: 1,
          rowsSeen: 12,
          currentDay: '2026-09-14',
        }),
      });
    } else {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          id: 'syncjob_e2e_1',
          status: 'COMPLETED',
          rangeFrom: '2026-09-14',
          rangeTo: '2026-09-14',
          pagesDone: 2,
          rowsSeen: 25,
          currentDay: '2026-09-14',
        }),
      });
    }
  });

  await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
    transactionFetches += 1;
    if (pollCalls >= 2) {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          items: [
            {
              id: 'tx-synced-1',
              semanticKey: 'ACB:1001',
              transactionDate: '2026-09-14 10:30:00',
              transactionDay: '2026-09-14',
              debit: 0,
              credit: 250000,
              description: 'TEST SYNC ACB COMPLETED',
              effectiveDate: '2026-09-14',
              firstSeenAt: '2026-09-14T10:30:00Z',
              source: 'FILTER_SYNC',
            },
          ],
          summary: { count: 1, incoming: 250000, outgoing: 0 },
        }),
      });
    } else {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items: [], summary: { count: 0, incoming: 0, outgoing: 0 } }),
      });
    }
  });

  await page.goto('/transactions');
  await page.getByRole('button', { name: 'Hôm nay' }).click();

  const syncBtn = page.getByRole('button', { name: 'Đồng bộ từ ACB' });
  await expect(syncBtn).toBeEnabled();
  await syncBtn.click();

  // Progress status banner appears
  const progressBanner = page.getByRole('status');
  await expect(progressBanner).toBeVisible();
  await expect(progressBanner).toContainText(/Đang đồng bộ|Đang chờ/);

  // Polls advance and completion notice is displayed
  await expect(page.getByText('Đã đồng bộ thành công 25 giao dịch từ ACB (2 trang).')).toBeVisible({ timeout: 10_000 });
  // Refetches transaction data with synced items
  await expect(page.getByText('TEST SYNC ACB COMPLETED')).toBeVisible();

  expect(ensureCalls).toBe(1);
  expect(pollCalls).toBeGreaterThanOrEqual(2);
});

test('resumes job polling on tab reload without creating duplicate sync jobs', async ({ page }) => {
  let ensureCalls = 0;
  let pollCalls = 0;

  await page.route('**/api/v1/transactions/ensure-history', async (route) => {
    ensureCalls += 1;
    await route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });

  await page.route('**/api/v1/transactions/history-sync-jobs/syncjob_resume_1', async (route) => {
    pollCalls += 1;
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'syncjob_resume_1',
        status: 'RUNNING',
        rangeFrom: '2026-09-08',
        rangeTo: '2026-09-14',
        pagesDone: 4,
        rowsSeen: 48,
        currentDay: '2026-09-10',
      }),
    });
  });

  await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [], summary: { count: 0, incoming: 0, outgoing: 0 } }),
    });
  });

  // Pre-seed active job in localStorage
  await page.goto('/transactions');
  await page.evaluate(() => {
    localStorage.setItem('acb_active_history_sync_job_id', 'syncjob_resume_1');
  });

  // Reload page to simulate closing/reopening tab
  await page.reload();

  // Progress status banner automatically appears and reflects ongoing progress
  const progressBanner = page.getByRole('status');
  await expect(progressBanner).toBeVisible();
  await expect(progressBanner).toContainText('Đang đồng bộ dữ liệu từ ngân hàng ACB...');
  await expect(progressBanner).toContainText('Số trang: 4');
  await expect(progressBanner).toContainText('Giao dịch đã nhận: 48');

  // No duplicate ensure-history POST calls made
  expect(ensureCalls).toBe(0);
  expect(pollCalls).toBeGreaterThanOrEqual(1);
});

test('allows explicit user cancellation of active sync job', async ({ page }) => {
  let cancelCalls = 0;

  await page.route('**/api/v1/transactions/ensure-history', async (route) => {
    await route.fulfill({
      status: 202,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'syncjob_cancel_1',
        status: 'RUNNING',
        coverage: 'PENDING',
        synced: false,
        job: {
          id: 'syncjob_cancel_1',
          status: 'RUNNING',
          rangeFrom: '2026-09-14',
          rangeTo: '2026-09-14',
          pagesDone: 1,
          rowsSeen: 10,
        },
      }),
    });
  });

  await page.route('**/api/v1/transactions/history-sync-jobs/syncjob_cancel_1', async (route) => {
    if (route.request().method() === 'DELETE') {
      cancelCalls += 1;
      await route.fulfill({
        status: 202,
        contentType: 'application/json',
        body: JSON.stringify({
          id: 'syncjob_cancel_1',
          status: 'CANCELED',
          pagesDone: 1,
          rowsSeen: 10,
        }),
      });
    } else {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          id: 'syncjob_cancel_1',
          status: cancelCalls > 0 ? 'CANCELED' : 'RUNNING',
          pagesDone: 1,
          rowsSeen: 10,
        }),
      });
    }
  });

  await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [], summary: { count: 0, incoming: 0, outgoing: 0 } }),
    });
  });

  await page.goto('/transactions');
  await page.getByRole('button', { name: 'Hôm nay' }).click();
  const syncBtn = page.getByRole('button', { name: 'Đồng bộ từ ACB' });
  await expect(syncBtn).toBeEnabled();
  await syncBtn.click();

  const cancelBtn = page.getByRole('button', { name: 'Hủy đồng bộ' });
  await expect(cancelBtn).toBeVisible();
  await cancelBtn.click();

  await expect(page.getByText('Đã hủy quá trình đồng bộ lịch sử ACB.')).toBeVisible();
  expect(cancelCalls).toBe(1);
});

test('handles automatic CSRF refresh on history sync mutation', async ({ page }) => {
  let postAttempts = 0;
  let csrfFetches = 0;

  await page.route('**/api/v1/csrf', async (route) => {
    csrfFetches += 1;
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ token: `fresh-token-${csrfFetches}` }),
    });
  });

  await page.route('**/api/v1/transactions/ensure-history', async (route) => {
    postAttempts += 1;
    if (postAttempts === 1) {
      await route.fulfill({
        status: 403,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'CSRF_TOKEN_INVALID', error: 'csrf token invalid' }),
      });
    } else {
      await route.fulfill({
        status: 202,
        contentType: 'application/json',
        body: JSON.stringify({
          id: 'syncjob_csrf_1',
          status: 'QUEUED',
          coverage: 'PENDING',
          synced: false,
          job: {
            id: 'syncjob_csrf_1',
            status: 'QUEUED',
            rangeFrom: '2026-09-14',
            rangeTo: '2026-09-14',
            pagesDone: 0,
            rowsSeen: 0,
          },
        }),
      });
    }
  });

  await page.route('**/api/v1/transactions/history-sync-jobs/syncjob_csrf_1', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'syncjob_csrf_1',
        status: 'RUNNING',
        pagesDone: 0,
        rowsSeen: 0,
      }),
    });
  });

  await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items: [], summary: { count: 0, incoming: 0, outgoing: 0 } }),
    });
  });

  await page.goto('/transactions');
  await page.getByRole('button', { name: 'Hôm nay' }).click();
  const syncBtn = page.getByRole('button', { name: 'Đồng bộ từ ACB' });
  await expect(syncBtn).toBeEnabled();
  await syncBtn.click();

  // Progress status banner appears despite the first attempt returning 403 CSRF_TOKEN_INVALID
  const progressBanner = page.getByRole('status');
  await expect(progressBanner).toBeVisible();
  await expect.poll(() => postAttempts).toBe(2);
});
