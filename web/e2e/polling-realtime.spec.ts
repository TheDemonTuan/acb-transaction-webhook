import { expect, test } from '@playwright/test';

test('updates polling runs from a server-sent event without reloading', async ({ page }) => {
  await page.addInitScript(() => {
    class TestEventSource extends EventTarget {
      static readonly CONNECTING = 0;
      static readonly OPEN = 1;
      static readonly CLOSED = 2;
      readonly CONNECTING = 0;
      readonly OPEN = 1;
      readonly CLOSED = 2;
      readyState = 1;
      withCredentials = false;
      onopen: ((event: Event) => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      onerror: ((event: Event) => void) | null = null;

      constructor(readonly url: string | URL) {
        super();
        (window as unknown as { __eventSources: TestEventSource[] }).__eventSources ??= [];
        (window as unknown as { __eventSources: TestEventSource[] }).__eventSources.push(this);
        queueMicrotask(() => this.onopen?.(new Event('open')));
      }

      close() {
        this.readyState = 2;
      }
    }

    Object.defineProperty(window, 'EventSource', { value: TestEventSource, configurable: true });
  });

  let pollRequests = 0;
  await page.route('**/api/v1/poll-runs**', async (route) => {
    pollRequests += 1;
    const items = pollRequests === 1
      ? []
      : [{
          id: 'poll_realtime',
          connectionId: 'conn_test',
          generation: 1,
          status: 'SUCCEEDED',
          pages: 1,
          rowsSeen: 7,
          startedAt: '2026-09-14T14:00:00Z',
          finishedAt: '2026-09-14T14:00:01Z',
        }];
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ items }),
    });
  });

  await page.goto('/admin/activity?tab=polling');
  await expect(page.getByText('Chưa có chu kỳ polling nào được ghi nhận.')).toBeVisible();

  await page.evaluate(() => {
    const event = new MessageEvent('poll.completed', {
      data: JSON.stringify({ status: 'SUCCEEDED', insertedCount: 0 }),
      lastEventId: 'ep1:1',
    });
    const sources = (window as unknown as { __eventSources: EventSource[] }).__eventSources;
    sources.at(-1)?.dispatchEvent(event);
  });

  await expect(page.getByText('Số dòng quét: 7')).toBeVisible();
  expect(pollRequests).toBeGreaterThanOrEqual(2);
});
