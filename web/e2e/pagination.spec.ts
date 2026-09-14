import { expect, test } from '@playwright/test';

test('activity page cursor pagination with next/prev, page size, and active tab', async ({ page }) => {
  const pollCalls: string[] = [];

  await page.route(/\/api\/v1\/poll-runs(?:\?.*)?$/, async (route) => {
    const url = new URL(route.request().url());
    const cursor = url.searchParams.get('cursor');
    const limit = url.searchParams.get('limit');
    pollCalls.push(`cursor=${cursor}&limit=${limit}`);

    if (!cursor) {
      // Page 1
      const items = Array.from({ length: 20 }, (_, i) => ({
        id: `poll_${i + 1}`,
        connectionId: 'conn_1',
        generation: 1,
        status: 'SUCCEEDED',
        pages: 1,
        rowsSeen: 10,
        startedAt: `2026-09-14T10:${String(i).padStart(2, '0')}:00Z`,
      }));
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items, nextCursor: 'poll_cursor_2' }),
      });
    } else if (cursor === 'poll_cursor_2') {
      // Page 2
      const items = Array.from({ length: 5 }, (_, i) => ({
        id: `poll_p2_${i + 1}`,
        connectionId: 'conn_1',
        generation: 1,
        status: 'SUCCEEDED',
        pages: 1,
        rowsSeen: 5,
        startedAt: `2026-09-14T09:${String(i).padStart(2, '0')}:00Z`,
      }));
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items }),
      });
    }
  });

  await page.goto('/admin/activity?tab=polling');

  // Page 1 assertions
  await expect(page.getByText('Trang 1 • 20 dòng')).toBeVisible();
  const prevBtn = page.getByRole('button', { name: 'Trước' });
  const nextBtn = page.getByRole('button', { name: 'Sau' });
  await expect(prevBtn).toBeDisabled();
  await expect(nextBtn).toBeEnabled();

  // Navigate to Page 2
  await nextBtn.click({ force: true });
  await expect(page.getByText('Trang 2 • 5 dòng')).toBeVisible();
  await expect(prevBtn).toBeEnabled();
  await expect(nextBtn).toBeDisabled();

  // Navigate back to Page 1
  await prevBtn.click({ force: true });
  await expect(page.getByText('Trang 1 • 20 dòng')).toBeVisible();
  await expect(prevBtn).toBeDisabled();
  await expect(nextBtn).toBeEnabled();
});

test('switching tabs resets cursor pagination to page 1', async ({ page }) => {
  await page.route(/\/api\/v1\/poll-runs(?:\?.*)?$/, async (route) => {
    const url = new URL(route.request().url());
    const cursor = url.searchParams.get('cursor');
    const items = Array.from({ length: cursor ? 5 : 20 }, (_, i) => ({
      id: `poll_${i + 1}`,
      connectionId: 'conn_1',
      generation: 1,
      status: 'SUCCEEDED',
      pages: 1,
      rowsSeen: 10,
      startedAt: '2026-09-14T10:00:00Z',
    }));
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items, nextCursor: cursor ? undefined : 'next_p' }),
    });
  });

  await page.route(/\/api\/v1\/deliveries(?:\?.*)?$/, async (route) => {
    const items = Array.from({ length: 20 }, (_, i) => ({
      id: `del_${i + 1}`,
      eventId: `ev_${i + 1}`,
      endpointId: 'ep_1',
      status: 'SUCCESS',
      attempts: 1,
      createdAt: '2026-09-14T10:00:00Z',
    }));
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ items, nextCursor: 'del_c2' }),
    });
  });

  await page.goto('/admin/activity?tab=polling');
  await expect(page.getByText('Trang 1 • 20 dòng')).toBeVisible();

  // Go to page 2 on polling
  await page.getByRole('button', { name: 'Sau' }).click({ force: true });
  await expect(page.getByText('Trang 2 • 5 dòng')).toBeVisible();

  // Switch to deliveries tab
  await page.getByRole('button', { name: 'Lịch sử gửi' }).click();

  // Deliveries should start on Page 1
  await expect(page.getByText('Trang 1 • 20 dòng')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Trước' })).toBeDisabled();
});

test('replay delivery on subsequent page updates record and preserves page position', async ({ page }) => {
  let replayed = false;

  await page.route(/\/api\/v1\/deliveries(?:\?.*)?$/, async (route) => {
    const url = new URL(route.request().url());
    const cursor = url.searchParams.get('cursor');

    if (!cursor) {
      const items = Array.from({ length: 20 }, (_, i) => ({
        id: `del_${i + 1}`,
        eventId: `ev_${i + 1}`,
        endpointId: 'ep_1',
        endpointName: 'Production Webhook',
        provider: 'WEBHOOK',
        status: 'SUCCESS',
        attempts: 1,
        createdAt: '2026-09-14T10:00:00Z',
      }));
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items, nextCursor: 'del_cursor_page2' }),
      });
    } else if (cursor === 'del_cursor_page2') {
      const items = [
        {
          id: 'del_dead_1',
          eventId: 'ev_dead_1',
          endpointId: 'ep_1',
          endpointName: 'Production Webhook',
          provider: 'WEBHOOK',
          status: replayed ? 'PENDING' : 'DEAD_LETTER',
          attempts: 5,
          createdAt: '2026-09-14T09:00:00Z',
        },
      ];
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items }),
      });
    }
  });

  await page.route('**/api/v1/deliveries/del_dead_1/replay', async (route) => {
    replayed = true;
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ status: 'PENDING' }),
    });
  });

  await page.goto('/admin/activity?tab=deliveries');

  // Go to page 2
  const nextBtn = page.getByRole('button', { name: 'Sau' });
  await expect(page.getByText('Trang 1 • 20 dòng')).toBeVisible();
  await nextBtn.click({ force: true });

  // On page 2: verify 1 item, DEAD_LETTER status, and Replay button
  await expect(page.getByText('Trang 2 • 1 dòng')).toBeVisible();
  const replayBtn = page.getByRole('button', { name: 'Gửi lại' });
  await expect(replayBtn).toBeVisible();

  // Click Replay
  await replayBtn.click();

  // Verify notification
  await expect(page.getByText('Đã đưa lượt phân phối trở lại hàng đợi gửi (PENDING).')).toBeVisible();

  // Verify page position is still Page 2 (did not reset to Page 1)
  await expect(page.getByText('Trang 2 • 1 dòng')).toBeVisible();
  const prevBtn = page.getByRole('button', { name: 'Trước' });
  await expect(prevBtn).toBeEnabled();
});

test('transactions page cursor pagination preserves summary stats and removes invalid remaining calculation', async ({ page }) => {
  await page.route(/\/api\/v1\/transactions(?:\?.*)?$/, async (route) => {
    const url = new URL(route.request().url());
    const cursor = url.searchParams.get('cursor');

    if (!cursor) {
      const items = Array.from({ length: 20 }, (_, i) => ({
        id: `tx_${i + 1}`,
        semanticKey: `key_${i + 1}`,
        accountNumber: '123456',
        amount: 50000,
        credit: 50000,
        debit: 0,
        description: `Thanh toan don hang #${i + 1}`,
        transactionDate: '2026-09-14T12:00:00Z',
        firstSeenAt: '2026-09-14T12:00:00Z',
      }));
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          items,
          nextCursor: 'tx_cursor_page2',
          summary: { count: 35, incoming: 1750000, outgoing: 0 },
        }),
      });
    } else {
      const items = Array.from({ length: 15 }, (_, i) => ({
        id: `tx_p2_${i + 1}`,
        semanticKey: `key_p2_${i + 1}`,
        accountNumber: '123456',
        amount: 50000,
        credit: 50000,
        debit: 0,
        description: `Thanh toan don hang p2 #${i + 1}`,
        transactionDate: '2026-09-14T11:00:00Z',
        firstSeenAt: '2026-09-14T11:00:00Z',
      }));
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          items,
          summary: { count: 35, incoming: 1750000, outgoing: 0 },
        }),
      });
    }
  });

  await page.goto('/transactions');

  // Verify backend summary is preserved in stats cards
  await expect(page.getByText('Tổng số giao dịch')).toBeVisible();
  await expect(page.getByText('35', { exact: true })).toBeVisible();

  // Verify pagination controls on page 1
  await expect(page.getByText('Trang 1 • 20 dòng')).toBeVisible();
  // Ensure the incorrect remaining calculation is gone
  await expect(page.getByText(/còn lại/)).not.toBeVisible();

  // Navigate to Page 2
  const nextBtn = page.getByRole('button', { name: 'Sau' });
  const prevBtn = page.getByRole('button', { name: 'Trước' });
  await nextBtn.click({ force: true });

  await expect(page.getByText('Trang 2 • 15 dòng')).toBeVisible();
  await expect(nextBtn).toBeDisabled();
  await expect(prevBtn).toBeEnabled();
  // Summary total is still 35
  await expect(page.getByText('35', { exact: true })).toBeVisible();
});
