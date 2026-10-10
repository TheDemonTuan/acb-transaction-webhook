import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
  toggleWebhookEndpoint,
  fetchSePayReviews,
} from './queries';
import { invalidateCsrfToken } from '../../api';
import { queryKeys } from './query-keys';

describe('queries and mutations with centralized CSRF', () => {
  const originalFetch = globalThis.fetch;

  beforeEach(() => {
    invalidateCsrfToken();
    vi.restoreAllMocks();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('toggleWebhookEndpoint sends POST with CSRF token', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === '/api/v1/csrf') {
        return new Response(JSON.stringify({ token: 'test-csrf-token' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === '/api/v1/webhooks/ep-1/disable') {
        const headers = new Headers(init?.headers);
        expect(headers.get('X-CSRF-Token')).toBe('test-csrf-token');
        return new Response(
          JSON.stringify({ id: 'ep-1', name: 'Webhook', url: 'https://example.com', status: 'DISABLED', revision: 1, provider: 'WEBHOOK', createdAt: '2026-09-14', updatedAt: '2026-09-14' }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        );
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const ep = await toggleWebhookEndpoint('ep-1', 'disable');
    expect(ep.status).toBe('DISABLED');
  });

  it('fetches sanitized SePay reviews with a private no-store GET and encoded cursor', async () => {
    const params = { cursor: 'opaque/cursor+value=', limit: 20 };
    const review = { storeKey: 'store', messageId: '9007199254740993', reason: 'INVALID_AMOUNT', receivedAt: '2026-10-10T12:00:00Z' };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ items: [review] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    }));
    globalThis.fetch = fetchMock;
    expect(await fetchSePayReviews(params)).toEqual({ items: [review] });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/sepay-reviews?cursor=opaque%2Fcursor%2Bvalue%3D&limit=20');
    expect(init.cache).toBe('no-store');
    expect(init.method ?? 'GET').toBe('GET');
    expect(init.body).toBeUndefined();
    expect(queryKeys.sepayReviews(params)).toEqual(['sepay-reviews', params]);
    expect(queryKeys.sepayReviews()).toEqual(['sepay-reviews', undefined]);
  });

  it('does not turn denied SePay review access into an empty successful page', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: 'Forbidden' }), {
      status: 403,
      headers: { 'Content-Type': 'application/json' },
    }));
    await expect(fetchSePayReviews()).rejects.toMatchObject({ status: 403 });
  });
});
