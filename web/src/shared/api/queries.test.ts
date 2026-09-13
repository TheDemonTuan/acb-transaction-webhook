import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
  ensureHistory,
  configureConnection,
  sendConnectionAction,
  startAuthSession,
  cancelAuthSession,
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

  it('ensureHistory sends POST with JSON body and auto-injected CSRF token', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === '/api/v1/csrf') {
        return new Response(JSON.stringify({ token: 'test-csrf-token' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === '/api/v1/transactions/ensure-history') {
        const headers = new Headers(init?.headers);
        expect(headers.get('X-CSRF-Token')).toBe('test-csrf-token');
        expect(headers.get('Content-Type')).toBe('application/json');
        expect(init?.body).toBe(JSON.stringify({ from: '2026-09-01', to: '2026-09-02' }));

        return new Response(
          JSON.stringify({ status: 'COMPLETE', coverage: 'FULL', synced: true, rowsSeen: 5 }),
          {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }
        );
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const res = await ensureHistory({ from: '2026-09-01', to: '2026-09-02' });
    expect(res.status).toBe('COMPLETE');
    expect(res.synced).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(2); // 1. /csrf, 2. /transactions/ensure-history
  });

  it('configureConnection sends POST with masked account and CSRF token', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === '/api/v1/csrf') {
        return new Response(JSON.stringify({ token: 'test-csrf-token' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === '/api/v1/connection/configure') {
        const headers = new Headers(init?.headers);
        expect(headers.get('X-CSRF-Token')).toBe('test-csrf-token');
        expect(headers.get('Content-Type')).toBe('application/json');
        return new Response(
          JSON.stringify({
            configured: true,
            connection: { id: 'conn-1', accountMasked: '***1234', state: 'CONFIGURED', generation: 1, updatedAt: '2026-09-14' },
          }),
          {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }
        );
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const conn = await configureConnection('***1234');
    expect(conn.configured).toBe(true);
    expect(conn.connection?.id).toBe('conn-1');
  });

  it('sendConnectionAction sends POST with CSRF token', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === '/api/v1/csrf') {
        return new Response(JSON.stringify({ token: 'test-csrf-token' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === '/api/v1/connection/pause') {
        const headers = new Headers(init?.headers);
        expect(headers.get('X-CSRF-Token')).toBe('test-csrf-token');
        return new Response(JSON.stringify({ status: 'PAUSED' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const result = await sendConnectionAction('pause');
    expect(result.status).toBe('PAUSED');
  });

  it('startAuthSession and cancelAuthSession auto-attach CSRF', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === '/api/v1/csrf') {
        return new Response(JSON.stringify({ token: 'test-csrf-token' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === '/api/v1/connection/auth/start') {
        const headers = new Headers(init?.headers);
        expect(headers.get('X-CSRF-Token')).toBe('test-csrf-token');
        return new Response(
          JSON.stringify({ attemptId: 'att-1', status: 'RUNNING', screenUrl: '/screen', expiresAt: '2026-09-14T03:00:00Z' }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        );
      }
      if (url === '/api/v1/connection/auth/cancel') {
        const headers = new Headers(init?.headers);
        expect(headers.get('X-CSRF-Token')).toBe('test-csrf-token');
        expect(headers.get('Content-Type')).toBe('application/json');
        return new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const startRes = await startAuthSession();
    expect(startRes.attemptId).toBe('att-1');

    await cancelAuthSession('att-1');
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
