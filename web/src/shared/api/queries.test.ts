import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
  ensureHistory,
  fetchHistorySyncJob,
  fetchLatestHistorySyncJob,
  cancelHistorySyncJob,
  configureConnection,
  sendConnectionAction,
  startAuthSession,
  cancelAuthSession,
  fetchCurrentAuthSession,
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
          JSON.stringify({
            status: 'QUEUED',
            coverage: 'PENDING',
            synced: false,
            job: {
              id: 'job-123',
              status: 'QUEUED',
              rangeFrom: '2026-09-01',
              rangeTo: '2026-09-02',
              pagesDone: 0,
              rowsSeen: 0,
            },
          }),
          {
            status: 202,
            headers: { 'Content-Type': 'application/json' },
          }
        );
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const res = await ensureHistory({ from: '2026-09-01', to: '2026-09-02' });
    expect(res.status).toBe('QUEUED');
    expect(res.synced).toBe(false);
    expect(res.job?.id).toBe('job-123');
    expect(fetchMock).toHaveBeenCalledTimes(2); // 1. /csrf, 2. /transactions/ensure-history
  });

  it('fetchHistorySyncJob performs GET for job descriptor', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string) => {
      if (url === '/api/v1/transactions/history-sync-jobs/job-123') {
        return new Response(
          JSON.stringify({
            id: 'job-123',
            status: 'RUNNING',
            rangeFrom: '2026-09-01',
            rangeTo: '2026-09-02',
            pagesDone: 2,
            rowsSeen: 15,
            currentDay: '2026-09-01',
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

    const job = await fetchHistorySyncJob('job-123');
    expect(job.id).toBe('job-123');
    expect(job.status).toBe('RUNNING');
    expect(job.pagesDone).toBe(2);
    expect(job.rowsSeen).toBe(15);
  });

  it('cancelHistorySyncJob sends DELETE with auto-injected CSRF token', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === '/api/v1/csrf') {
        return new Response(JSON.stringify({ token: 'test-csrf-token' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === '/api/v1/transactions/history-sync-jobs/job-123') {
        expect(init?.method).toBe('DELETE');
        const headers = new Headers(init?.headers);
        expect(headers.get('X-CSRF-Token')).toBe('test-csrf-token');
        return new Response(
          JSON.stringify({
            id: 'job-123',
            status: 'CANCELED',
            rangeFrom: '2026-09-01',
            rangeTo: '2026-09-02',
            pagesDone: 2,
            rowsSeen: 15,
          }),
          {
            status: 202,
            headers: { 'Content-Type': 'application/json' },
          }
        );
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const canceled = await cancelHistorySyncJob('job-123');
    expect(canceled.id).toBe('job-123');
    expect(canceled.status).toBe('CANCELED');
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

  it('fetchCurrentAuthSession preserves browserUnavailable flag when reported by gateway', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string) => {
      if (url === '/api/v1/connection/auth/current') {
        return new Response(
          JSON.stringify({
            attempt: {
              attemptId: 'att-transient-1',
              status: 'STARTING',
              screenUrl: '/api/v1/connection/auth/att-transient-1/screen/vnc.html',
              expiresAt: '2026-09-14T18:00:00Z',
              browserUnavailable: true,
            },
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        );
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const res = await fetchCurrentAuthSession();
    expect(res.attempt).not.toBeNull();
    expect(res.attempt?.attemptId).toBe('att-transient-1');
    expect(res.attempt?.browserUnavailable).toBe(true);
  });

  it('fetchCurrentAuthSession handles ready attempt without browserUnavailable', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string) => {
      if (url === '/api/v1/connection/auth/current') {
        return new Response(
          JSON.stringify({
            attempt: {
              attemptId: 'att-ready-1',
              status: 'AWAITING_USER_LOGIN',
              screenUrl: '/api/v1/connection/auth/att-ready-1/screen/vnc.html',
              expiresAt: '2026-09-14T18:00:00Z',
            },
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        );
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const res = await fetchCurrentAuthSession();
    expect(res.attempt).not.toBeNull();
    expect(res.attempt?.attemptId).toBe('att-ready-1');
    expect(res.attempt?.browserUnavailable).toBeUndefined();
  });
});
