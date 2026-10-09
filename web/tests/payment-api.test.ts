import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../src/app/runtime-mode', () => ({ isPublicViewerHost: vi.fn(() => false) }));

import { api, invalidateCsrfToken, publicApi } from '../src/api';
import { isPublicViewerHost } from '../src/app/runtime-mode';
import {
  cancelPaymentOrder,
  confirmPaymentWebhook,
  createPaymentOrder,
  createPublicPaymentOrder,
  fetchPaymentConfig,
  fetchPaymentOrders,
  fetchPaymentReviews,
  fetchPublicPaymentOrder,
} from '../src/shared/api/queries';

const key = '3e628a46-7db9-4f7b-a95f-d9763e2af3de';
const id = 'A'.repeat(43);
const order = {
  id,
  orderCode: '100000000001',
  amountVnd: 50_000,
  origin: 'STATIC_URL',
  status: 'PENDING',
  createdAt: '2026-10-08T00:00:00Z',
  expiresAt: '2026-10-08T00:30:00Z',
};

let requests: { url: string; init?: RequestInit }[];

beforeEach(() => {
  invalidateCsrfToken();
  vi.mocked(isPublicViewerHost).mockReturnValue(false);
  requests = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    requests.push({ url, init });
    const body = url === '/api/v1/csrf' ? { token: 'csrf-test-token' } : order;
    return new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
  }));
});

afterEach(() => {
  vi.unstubAllGlobals();
  invalidateCsrfToken();
});

describe('public payment HTTP client', () => {
  it('uses the public API even when localhost/admin host detection says admin', async () => {
    expect(await createPublicPaymentOrder(50_000, 'STATIC_URL', key)).toEqual(order);
    await fetchPaymentConfig();
    await fetchPublicPaymentOrder(id);
    expect(requests.map((request) => request.url)).toEqual([
      '/api/public/v1/payments', '/api/public/v1/payment-config', `/api/public/v1/payments/${id}`,
    ]);
    expect(requests.every((request) => request.init?.cache === 'no-store')).toBe(true);
    const init = requests[0].init!;
    const headers = new Headers(init.headers);
    expect(headers.get('Content-Type')).toBe('application/json');
    expect(headers.get('Idempotency-Key')).toBe(key);
    expect(headers.has('X-CSRF-Token')).toBe(false);
    expect(JSON.parse(init.body as string)).toEqual({ amountVnd: 50_000, origin: 'STATIC_URL' });
  });

  it('accepts only exact POST /payments anonymously, not cancel/confirm/prefixes', async () => {
    for (const [path, method] of [
      ['/payments', 'DELETE'],
      ['/payments/', 'POST'],
      ['/payments?amount=50', 'POST'],
      [`/payments/${id}/cancel`, 'POST'],
      ['/payment-provider/confirm-webhook', 'POST'],
      ['/payment-activity', 'POST'],
    ]) {
      await expect(publicApi(path, { method })).rejects.toMatchObject({ status: 405, code: 'PUBLIC_READ_ONLY' });
    }
    expect(requests).toHaveLength(0);
    vi.mocked(isPublicViewerHost).mockReturnValue(true);
    await expect(api(`/payments/${id}/cancel`, { method: 'POST' })).rejects.toMatchObject({ status: 405 });
    await api('/payments', { method: 'POST', body: '{}' });
    expect(requests.map((request) => request.url)).toEqual(['/api/public/v1/payments']);
  });

  it('rejects noninteger/nonpositive amounts and invalid keys before making a request', async () => {
    for (const amount of [0, -50, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      await expect(createPublicPaymentOrder(amount, 'STATIC_URL', key)).rejects.toMatchObject({ code: 'INVALID_AMOUNT' });
    }
    await expect(createPublicPaymentOrder(50_000, 'STATIC_URL', 'not-a-uuid')).rejects.toMatchObject({ code: 'INVALID_IDEMPOTENCY_KEY' });
    expect(requests).toHaveLength(0);
  });

  it('preserves the same key and amount when an explicit caller retries after a network loss', async () => {
    vi.mocked(fetch).mockRejectedValueOnce(new TypeError('network unavailable'));
    await expect(createPublicPaymentOrder(50_000, 'OPERATOR_DYNAMIC', key)).rejects.toThrow();
    await createPublicPaymentOrder(50_000, 'OPERATOR_DYNAMIC', key);
    const calls = vi.mocked(fetch).mock.calls;
    expect(calls).toHaveLength(2);
    expect(new Headers(calls[0][1]?.headers).get('Idempotency-Key')).toBe(key);
    expect(new Headers(calls[1][1]?.headers).get('Idempotency-Key')).toBe(key);
    expect(calls[0][1]?.body).toBe(calls[1][1]?.body);
  });

  it('surfaces sanitized server conflict codes without generating another request', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(new Response(JSON.stringify({ error: 'Intent conflict', code: 'IDEMPOTENCY_CONFLICT' }), {
      status: 409, headers: { 'Content-Type': 'application/json' },
    }));
    await expect(createPublicPaymentOrder(50_000, 'STATIC_URL', key)).rejects.toMatchObject({ status: 409, code: 'IDEMPOTENCY_CONFLICT' });
    expect(vi.mocked(fetch)).toHaveBeenCalledTimes(1);
  });
});

describe('admin payment HTTP client', () => {
  it('retains CSRF and excludes browser-controlled origin/URLs from admin create', async () => {
    await createPaymentOrder(50_000, key);
    await cancelPaymentOrder(id);
    await confirmPaymentWebhook();
    expect(requests.map((request) => request.url)).toEqual([
      '/api/v1/csrf', '/api/v1/payments', `/api/v1/payments/${id}/cancel`, '/api/v1/payment-provider/confirm-webhook',
    ]);
    for (const request of requests.slice(1)) {
      expect(new Headers(request.init?.headers).get('X-CSRF-Token')).toBe('csrf-test-token');
    }
    expect(JSON.parse(requests[1].init?.body as string)).toEqual({ amountVnd: 50_000 });
    expect(requests[3].init?.body).toBeUndefined();
  });

  it('uses one status filter and existing cursor/limit pagination for orders/reviews', async () => {
    await fetchPaymentOrders({ status: 'CREATING', cursor: 'next /page', limit: 100 });
    await fetchPaymentReviews({ cursor: 'review /page', limit: 25 });
    expect(requests.map((request) => request.url)).toEqual([
      '/api/v1/payments?status=CREATING&cursor=next+%2Fpage&limit=100',
      '/api/v1/payment-reviews?cursor=review+%2Fpage&limit=25',
    ]);
  });
});
