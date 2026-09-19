import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { isPublicViewerHost, PUBLIC_VIEWER_HOST, ADMIN_ORIGIN } from '../src/app/runtime-mode';
import { publicRoutes, adminRoutes } from '../src/app/router';
import { api, apiAudio } from '../src/api';
import { buildSingleTransactionPhrase } from '../src/features/voice-announcements/voice-copy';

describe('Runtime Mode & Public Isolation', () => {
  let prevWindow: any;

  beforeEach(() => {
    prevWindow = (globalThis as any).window;
  });

  afterEach(() => {
    (globalThis as any).window = prevWindow;
    vi.restoreAllMocks();
  });

  it('correctly identifies public viewer hostname', () => {
    expect(isPublicViewerHost(PUBLIC_VIEWER_HOST)).toBe(true);
    expect(isPublicViewerHost('bank.tuannguyenviet.site')).toBe(false);
    expect(isPublicViewerHost('localhost')).toBe(false);
    expect(isPublicViewerHost('')).toBe(false);
  });

  it('public routes do not contain admin paths', () => {
    const findAdmin = (routes: any[]): boolean => {
      for (const r of routes) {
        if (typeof r.path === 'string' && r.path.includes('admin')) return true;
        if (r.children && findAdmin(r.children)) return true;
      }
      return false;
    };

    expect(findAdmin(publicRoutes)).toBe(false);
    expect(findAdmin(adminRoutes)).toBe(true);
  });

  describe('API behavior on public host', () => {
    beforeEach(() => {
      (globalThis as any).window = {
        location: {
          hostname: PUBLIC_VIEWER_HOST,
          origin: `https://${PUBLIC_VIEWER_HOST}`,
        },
      };
    });

    it('blocks mutations (POST/PUT/PATCH/DELETE) on public host without network call', async () => {
      const fetchSpy = vi.spyOn(globalThis, 'fetch');

      await expect(api('/transactions', { method: 'POST' })).rejects.toThrow(
        'Trang xem giao dịch chỉ hỗ trợ đọc dữ liệu.',
      );
      await expect(api('/monitor/settings', { method: 'PUT' })).rejects.toThrow(
        'Trang xem giao dịch chỉ hỗ trợ đọc dữ liệu.',
      );
      await expect(api('/transactions/history-sync-jobs/1', { method: 'DELETE' })).rejects.toThrow(
        'Trang xem giao dịch chỉ hỗ trợ đọc dữ liệu.',
      );

      expect(fetchSpy).not.toHaveBeenCalled();
    });

    it('blocks arbitrary apiAudio on public host and allows transaction-scoped audio', async () => {
      const fetchSpy = vi.spyOn(globalThis, 'fetch');
      await expect(apiAudio('/voice/test')).rejects.toThrow(
        'Trang xem giao dịch chỉ hỗ trợ đọc dữ liệu.',
      );
      expect(fetchSpy).not.toHaveBeenCalled();

      const mockFetch = vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce({
        ok: true,
        headers: new Headers({ 'content-type': 'audio/mpeg', 'x-tts-provider': 'edge' }),
        arrayBuffer: async () => new ArrayBuffer(4),
      } as Response);

      await apiAudio('/voice/transactions/1');
      expect(mockFetch).toHaveBeenCalledTimes(1);
      expect(mockFetch.mock.calls[0][0]).toBe('/api/public/v1/voice/transactions/1');
    });

    it('routes GET requests to /api/public/v1 namespace', async () => {
      const mockFetch = vi.spyOn(globalThis, 'fetch').mockResolvedValueOnce({
        ok: true,
        headers: new Headers({ 'content-type': 'application/json' }),
        json: async () => ({ items: [] }),
      } as Response);

      const res = await api<{ items: any[] }>('/transactions');
      expect(mockFetch).toHaveBeenCalledTimes(1);
      const url = mockFetch.mock.calls[0][0];
      expect(url).toBe('/api/public/v1/transactions');
      expect(res.items).toEqual([]);
    });

    it('builds proper speech phrase for public viewer without calling audio replay API', () => {
      const phrase = buildSingleTransactionPhrase('50000', 'ung ho quy', { includeDescription: true });
      expect(phrase).toContain('năm mươi nghìn đồng');
      expect(phrase).toContain('ung ho quy');
    });
  });
});
