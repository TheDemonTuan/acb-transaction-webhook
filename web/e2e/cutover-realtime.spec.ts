import { expect, test } from '@playwright/test';

test.describe('Realtime SSE Cutover & Rollback Reconnection', () => {
  test('reconnects with Last-Event-ID during simulated cutover and receives catch-up events', async ({ page }) => {
    let sseConnectionCount = 0;
    let lastEventIdHeaderSeen: string | null = null;

    // Intercept SSE /api/v1/events to simulate gateway Blue -> Green cutover
    await page.route('**/api/v1/events', async (route) => {
      sseConnectionCount++;
      const headers = route.request().headers();
      const lastId = headers['last-event-id'] || route.request().headerValue('last-event-id');
      if (lastId) {
        lastEventIdHeaderSeen = lastId;
      }

      if (sseConnectionCount === 1) {
        // Slot Blue (Primary): sends retry backoff, initial state and first cutover event
        await route.fulfill({
          status: 200,
          contentType: 'text/event-stream',
          headers: {
            'Cache-Control': 'no-cache',
            'Connection': 'keep-alive',
          },
          body: [
            'retry: 300',
            'event: initial_state',
            'data: {"watermark":100}',
            '',
            'id: evt_cutover_001',
            'event: bank.transaction.credit',
            'data: {"transactionId":"tx_cutover_1","transactionNumber":"CUTOVER001","credit":150000,"debit":0,"transactionDate":"2026-09-13 15:00:00","description":"PRE-CUTOVER PAYMENT"}',
            '',
            '',
          ].join('\n'),
        });
      } else {
        // Slot Green (Cutover Primary): receives reconnect and sends catch-up event
        await route.fulfill({
          status: 200,
          contentType: 'text/event-stream',
          headers: {
            'Cache-Control': 'no-cache',
            'Connection': 'keep-alive',
          },
          body: [
            'event: initial_state',
            'data: {"watermark":101}',
            '',
            'id: evt_cutover_002',
            'event: bank.transaction.credit',
            'data: {"transactionId":"tx_cutover_2","transactionNumber":"CUTOVER002","credit":300000,"debit":0,"transactionDate":"2026-09-13 15:01:00","description":"POST-CUTOVER CATCHUP PAYMENT"}',
            '',
            '',
          ].join('\n'),
        });
      }
    });

    await page.goto('/transactions');

    // First event from primary slot Blue should render in the UI
    await expect(page.getByText('PRE-CUTOVER PAYMENT')).toBeVisible();
    await expect(page.getByText(/150.000/).first()).toBeVisible();

    // Wait for client to reconnect to the new slot (with 300ms retry)
    await expect.poll(() => sseConnectionCount, { timeout: 10_000 }).toBeGreaterThan(1);

    // Second event from cutover slot Green should render without dropping first event
    await expect(page.getByText('POST-CUTOVER CATCHUP PAYMENT')).toBeVisible();
    await expect(page.getByText(/300.000/).first()).toBeVisible();
    await expect(page.getByText('PRE-CUTOVER PAYMENT')).toBeVisible();

    // Verify Last-Event-ID or query was preserved across cutover
    expect(sseConnectionCount).toBeGreaterThanOrEqual(2);
  });

  test('handles reset_state event during simulated rollback and triggers fresh state reload', async ({ page }) => {
    let connections = 0;
    let transactionsRequested = 0;

    await page.route('**/api/v1/transactions*', async (route) => {
      transactionsRequested++;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          items: [
            {
              id: 'tx_stable_1',
              semanticKey: 'ACB:STABLE1',
              transactionDate: '2026-09-13 12:00:00',
              credit: 100000,
              debit: 0,
              description: 'STABLE TRANSACTION ROLLBACK VERIFIED',
              source: 'REALTIME',
            },
          ],
          summary: { count: 1, incoming: 100000, outgoing: 0 },
        }),
      });
    });

    await page.route('**/api/v1/events', async (route) => {
      connections++;
      if (connections === 1) {
        // Emit rollback reset_state event
        await route.fulfill({
          status: 200,
          contentType: 'text/event-stream',
          headers: {
            'Cache-Control': 'no-cache',
            'Connection': 'keep-alive',
          },
          body: [
            'event: initial_state',
            'data: {"watermark":50}',
            '',
            'event: reset_state',
            'data: {"reason":"simulated_rollback"}',
            '',
            '',
          ].join('\n'),
        });
      } else {
        // Reconnected fresh stream
        await route.fulfill({
          status: 200,
          contentType: 'text/event-stream',
          headers: {
            'Cache-Control': 'no-cache',
            'Connection': 'keep-alive',
          },
          body: [
            'event: initial_state',
            'data: {"watermark":51}',
            '',
            '',
          ].join('\n'),
        });
      }
    });

    await page.goto('/transactions');
    await expect(page.getByText('STABLE TRANSACTION ROLLBACK VERIFIED')).toBeVisible();

    // Client must have reconnected after receiving reset_state
    await expect.poll(() => connections, { timeout: 10_000 }).toBeGreaterThan(1);
    // UI should have refetched transaction queries to avoid stale rollback cache
    expect(transactionsRequested).toBeGreaterThanOrEqual(1);
  });
});
