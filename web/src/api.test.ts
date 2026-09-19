import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { api, apiAudio, isMutation, ApiError, CSRF_CODE_TOKEN_INVALID, invalidateCsrfToken } from './api';
import * as runtimeMode from './app/runtime-mode';

describe('api transport and centralized CSRF', () => {
  const originalFetch = globalThis.fetch;

  beforeEach(() => {
    invalidateCsrfToken();
    vi.restoreAllMocks();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('identifies mutation methods accurately', () => {
    expect(isMutation('POST')).toBe(true);
    expect(isMutation('post')).toBe(true);
    expect(isMutation('PUT')).toBe(true);
    expect(isMutation('put')).toBe(true);
    expect(isMutation('PATCH')).toBe(true);
    expect(isMutation('patch')).toBe(true);
    expect(isMutation('DELETE')).toBe(true);
    expect(isMutation('delete')).toBe(true);

    expect(isMutation('GET')).toBe(false);
    expect(isMutation('get')).toBe(false);
    expect(isMutation('HEAD')).toBe(false);
    expect(isMutation('OPTIONS')).toBe(false);
  });

  it('GET and HEAD requests do not request a CSRF token', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string) => {
      if (url === '/api/v1/test') {
        return new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const res = await api<{ ok: boolean }>('/test');
    expect(res).toEqual({ ok: true });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith('/api/v1/test', expect.objectContaining({
      credentials: 'same-origin',
    }));

    // Verify X-CSRF-Token was not attached
    const calledInit = fetchMock.mock.calls[0][1] as RequestInit;
    const headers = new Headers(calledInit.headers);
    expect(headers.has('X-CSRF-Token')).toBe(false);
  });

  it.each(['POST', 'PUT', 'PATCH', 'DELETE'])(
    '%s requests CSRF token and injects X-CSRF-Token while preserving caller headers',
    async (method) => {
      const calls: string[] = [];
      const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
        calls.push(`${init?.method ?? 'GET'} ${url}`);
        if (url === '/api/v1/csrf') {
          return new Response(JSON.stringify({ token: 'mock-csrf-token' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
        }
        if (url === '/api/v1/resource') {
          const headers = new Headers(init?.headers);
          expect(headers.get('X-CSRF-Token')).toBe('mock-csrf-token');
          expect(headers.get('X-Custom-Header')).toBe('custom-value');
          expect(headers.get('Content-Type')).toBe('application/json');
          return new Response(JSON.stringify({ created: true }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
        }
        throw new Error(`Unexpected url: ${url}`);
      });
      globalThis.fetch = fetchMock;

      const res = await api<{ created: true }>('/resource', {
        method,
        headers: {
          'Content-Type': 'application/json',
          'X-Custom-Header': 'custom-value',
        },
        body: JSON.stringify({ data: 123 }),
      });

      expect(res).toEqual({ created: true });
      expect(calls).toEqual(['GET /api/v1/csrf', `${method} /api/v1/resource`]);
    }
  );

  it('CSRF_TOKEN_INVALID triggers one forced token refresh and one retry only', async () => {
    let attempt = 0;
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url === '/api/v1/csrf') {
        attempt++;
        return new Response(JSON.stringify({ token: `token-v${attempt}` }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === '/api/v1/mutate') {
        const headers = new Headers(init?.headers);
        const token = headers.get('X-CSRF-Token');
        if (token === 'token-v1') {
          return new Response(
            JSON.stringify({ error: 'invalid csrf token', code: CSRF_CODE_TOKEN_INVALID }),
            {
              status: 403,
              headers: { 'Content-Type': 'application/json' },
            }
          );
        }
        if (token === 'token-v2') {
          return new Response(JSON.stringify({ success: true }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
        }
      }
      throw new Error(`Unexpected call: ${url}`);
    });
    globalThis.fetch = fetchMock;

    const res = await api<{ success: boolean }>('/mutate', {
      method: 'POST',
      body: JSON.stringify({ action: 'run' }),
    });

    expect(res).toEqual({ success: true });
    // Expect: 1. GET /csrf (token-v1) -> 2. POST /mutate (403 invalid) -> 3. GET /csrf (token-v2) -> 4. POST /mutate (200 ok)
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  it('fails permanently if retry on CSRF_TOKEN_INVALID also fails (bounded to 1 retry)', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string) => {
      if (url === '/api/v1/csrf') {
        return new Response(JSON.stringify({ token: 'expired-token' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (url === '/api/v1/mutate') {
        return new Response(
          JSON.stringify({ error: 'invalid token always', code: CSRF_CODE_TOKEN_INVALID }),
          {
            status: 403,
            headers: { 'Content-Type': 'application/json' },
          }
        );
      }
      throw new Error(`Unexpected url: ${url}`);
    });
    globalThis.fetch = fetchMock;

    await expect(
      api('/mutate', {
        method: 'POST',
      })
    ).rejects.toThrow();

    // 1st /csrf, 1st /mutate (failed), 2nd /csrf (forced refresh), 2nd /mutate (failed) -> throws!
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  it.each([400, 401, 403, 409, 500])(
    'ordinary %d errors are not automatically replayed',
    async (statusCode) => {
      const mutateCalls: string[] = [];
      const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
        if (url === '/api/v1/csrf') {
          return new Response(JSON.stringify({ token: 'csrf-ok' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
        }
        if (url === '/api/v1/mutate') {
          mutateCalls.push(url);
          return new Response(
            JSON.stringify({ error: `status ${statusCode} error`, code: 'CUSTOM_ERROR' }),
            {
              status: statusCode,
              headers: { 'Content-Type': 'application/json' },
            }
          );
        }
        throw new Error(`Unexpected call: ${url}`);
      });
      globalThis.fetch = fetchMock;

      await expect(
        api('/mutate', {
          method: 'POST',
          body: JSON.stringify({ item: 1 }),
        })
      ).rejects.toThrow(ApiError);

      // Mutate endpoint was called exactly once — no replay!
      expect(mutateCalls).toHaveLength(1);
    }
  );

  it('translates 429 into friendly Vietnamese message', async () => {
    const fetchMock = vi.fn().mockImplementation(async () => {
      return new Response(JSON.stringify({ error: 'Too Many Requests' }), {
        status: 429,
        headers: { 'Content-Type': 'application/json' },
      });
    });
    globalThis.fetch = fetchMock;

    await expect(api('/test-rate-limit')).rejects.toMatchObject({
      status: 429,
      message: 'Có quá nhiều yêu cầu cùng lúc. Vui lòng đợi một chút rồi thử lại.',
    });
  });

  it('preserves and propagates AbortSignal', async () => {
    const controller = new AbortController();
    controller.abort();

    const fetchMock = vi.fn().mockImplementation(async (_url: string, init?: RequestInit) => {
      if (init?.signal?.aborted) {
        const err = new Error('The operation was aborted.');
        err.name = 'AbortError';
        throw err;
      }
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    });
    globalThis.fetch = fetchMock;

    await expect(
      api('/test', {
        signal: controller.signal,
      })
    ).rejects.toThrowError(expect.objectContaining({ name: 'AbortError' }));
  });

  it('apiAudio routes to /api/public/v1/voice/transactions/ on public host', async () => {
    vi.spyOn(runtimeMode, 'isPublicViewerHost').mockReturnValue(true);

    let requestedUrl = '';
    const fetchMock = vi.fn(async (url: any) => {
      requestedUrl = String(url);
      return new Response(new ArrayBuffer(4), {
        status: 200,
        headers: {
          'Content-Type': 'audio/mpeg',
          'X-TTS-Provider': 'edge',
          'X-TTS-Voice': 'vi-VN-HoaiMyNeural',
        },
      });
    });
    globalThis.fetch = fetchMock;

    const res = await apiAudio('/voice/transactions/txn_public_1');
    expect(requestedUrl).toBe('/api/public/v1/voice/transactions/txn_public_1');
    expect(res.provider).toBe('edge');
  });

  it('apiAudio routes /voice/test to /api/public/v1/voice/test on public host', async () => {
    vi.spyOn(runtimeMode, 'isPublicViewerHost').mockReturnValue(true);

    let requestedUrl = '';
    const fetchMock = vi.fn(async (url: any) => {
      requestedUrl = String(url);
      return new Response(new ArrayBuffer(4), {
        status: 200,
        headers: {
          'Content-Type': 'audio/mpeg',
          'X-TTS-Provider': 'edge',
          'X-TTS-Voice': 'vi-VN-HoaiMyNeural',
        },
      });
    });
    globalThis.fetch = fetchMock;

    const res = await apiAudio('/voice/test', {
      method: 'POST',
      body: JSON.stringify({ voiceId: 'vi-VN-HoaiMyNeural' }),
    });
    expect(requestedUrl).toBe('/api/public/v1/voice/test');
    expect(res.provider).toBe('edge');
  });

  it('apiAudio rejects arbitrary non-voice routes on public host', async () => {
    vi.spyOn(runtimeMode, 'isPublicViewerHost').mockReturnValue(true);

    await expect(apiAudio('/voice/settings')).rejects.toThrow(
      'Trang xem giao dịch chỉ hỗ trợ đọc dữ liệu.'
    );
  });
});
