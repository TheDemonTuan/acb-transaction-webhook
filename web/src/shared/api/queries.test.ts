import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
  toggleWebhookEndpoint,
} from './queries';
import { invalidateCsrfToken } from '../../api';

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
});
