import { describe, it, expect } from 'vitest';
import {
  parseAmountThousandsToVnd,
  computeBoostPhase,
  isCreditMatch,
} from '../src/features/payment-qr/ReceivingQRModal';
import { getDynamicPaymentQRURL } from '../src/shared/api/queries';

describe('QR Modal Payment Boost and Workflow Helpers', () => {
  describe('parseAmountThousandsToVnd', () => {
    it('correctly converts thousands shorthand to VND', () => {
      expect(parseAmountThousandsToVnd('100')).toBe(100_000);
      expect(parseAmountThousandsToVnd('250')).toBe(250_000);
      expect(parseAmountThousandsToVnd('1000')).toBe(1_000_000);
    });

    it('handles empty, zero, and non-numeric inputs gracefully', () => {
      expect(parseAmountThousandsToVnd('')).toBe(0);
      expect(parseAmountThousandsToVnd('0')).toBe(0);
      expect(parseAmountThousandsToVnd('000')).toBe(0);
      expect(parseAmountThousandsToVnd('abc')).toBe(0);
      expect(parseAmountThousandsToVnd('100k')).toBe(100_000);
    });
  });

  describe('computeBoostPhase', () => {
    it('assigns Phase 1 (1–3s) during the first 60 seconds (121–180s remaining)', () => {
      const p1 = computeBoostPhase(180);
      expect(p1.phase).toBe(1);
      expect(p1.minSec).toBe(1);
      expect(p1.maxSec).toBe(3);
      expect(p1.label).toContain('1–3 giây');

      const p1End = computeBoostPhase(121);
      expect(p1End.phase).toBe(1);
    });

    it('assigns Phase 2 (3–6s) during the second minute (61–120s remaining)', () => {
      const p2Start = computeBoostPhase(120);
      expect(p2Start.phase).toBe(2);
      expect(p2Start.minSec).toBe(3);
      expect(p2Start.maxSec).toBe(6);
      expect(p2Start.label).toContain('3–6 giây');

      const p2End = computeBoostPhase(61);
      expect(p2End.phase).toBe(2);
    });

    it('assigns Phase 3 (6–10s) during the third minute (1–60s remaining)', () => {
      const p3Start = computeBoostPhase(60);
      expect(p3Start.phase).toBe(3);
      expect(p3Start.minSec).toBe(6);
      expect(p3Start.maxSec).toBe(10);
      expect(p3Start.label).toContain('6–10 giây');

      const p3End = computeBoostPhase(1);
      expect(p3End.phase).toBe(3);
    });

    it('falls back to standard interval (20–30s) when boost has expired (0s remaining)', () => {
      const pExpired = computeBoostPhase(0);
      expect(pExpired.phase).toBe(0);
      expect(pExpired.minSec).toBe(20);
      expect(pExpired.maxSec).toBe(30);
      expect(pExpired.label).toContain('20–30 giây');
    });
  });

  describe('isCreditMatch', () => {
    it('matches exact amount when expected amount is specified', () => {
      expect(isCreditMatch(100_000, 100_000)).toBe(true);
      expect(isCreditMatch(100_000, 50_000)).toBe(false);
      expect(isCreditMatch(250_000, 250_000)).toBe(true);
      expect(isCreditMatch(250_000, 250_001)).toBe(false);
    });

    it('matches any positive credit when expected amount is 0 (unfixed amount)', () => {
      expect(isCreditMatch(0, 50_000)).toBe(true);
      expect(isCreditMatch(0, 1_000_000)).toBe(true);
      expect(isCreditMatch(0, 0)).toBe(false);
      expect(isCreditMatch(0, -10_000)).toBe(false);
    });
  });

  describe('getDynamicPaymentQRURL', () => {
    it('appends amount query parameter when amount is greater than 0', () => {
      expect(getDynamicPaymentQRURL(100_000)).toBe('/api/public/v1/payment-qr/image?amount=100000');
      expect(getDynamicPaymentQRURL(500_000)).toBe('/api/public/v1/payment-qr/image?amount=500000');
    });

    it('returns base QR URL when amount is 0 or undefined', () => {
      expect(getDynamicPaymentQRURL(0)).toBe('/api/public/v1/payment-qr/image');
      expect(getDynamicPaymentQRURL()).toBe('/api/public/v1/payment-qr/image');
    });
  });

  describe('stopPaymentActivity helper', () => {
    it('calls DELETE /api/public/v1/payment-activity without sessionId', async () => {
      const origFetch = globalThis.fetch;
      let requestedUrl = '';
      let requestedMethod = '';
      globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
        requestedUrl = String(input);
        requestedMethod = init?.method || 'GET';
        return new Response(JSON.stringify({ ok: true }), { status: 200 });
      }) as typeof fetch;

      const { stopPaymentActivity } = await import('../src/shared/api/queries');
      await stopPaymentActivity();

      expect(requestedUrl).toBe('/api/public/v1/payment-activity');
      expect(requestedMethod).toBe('DELETE');

      globalThis.fetch = origFetch;
    });

    it('calls DELETE /api/public/v1/payment-activity?sessionId=... when sessionId provided', async () => {
      const origFetch = globalThis.fetch;
      let requestedUrl = '';
      let requestedMethod = '';
      globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
        requestedUrl = String(input);
        requestedMethod = init?.method || 'GET';
        return new Response(JSON.stringify({ ok: true }), { status: 200 });
      }) as typeof fetch;

      const { stopPaymentActivity } = await import('../src/shared/api/queries');
      await stopPaymentActivity('session_abc_123');

      expect(requestedUrl).toBe('/api/public/v1/payment-activity?sessionId=session_abc_123');
      expect(requestedMethod).toBe('DELETE');

      globalThis.fetch = origFetch;
    });
  });

  describe('fetchPaymentReadiness helper', () => {
    it('calls GET /api/public/v1/payment-readiness and returns status', async () => {
      const origFetch = globalThis.fetch;
      let requestedUrl = '';
      globalThis.fetch = (async (input: RequestInfo | URL) => {
        requestedUrl = String(input);
        return new Response(JSON.stringify({ ready: true, status: 'READY' }), { status: 200 });
      }) as typeof fetch;

      const { fetchPaymentReadiness } = await import('../src/shared/api/queries');
      const res = await fetchPaymentReadiness();

      expect(requestedUrl).toBe('/api/public/v1/payment-readiness');
      expect(res.ready).toBe(true);
      expect(res.status).toBe('READY');

      globalThis.fetch = origFetch;
    });
  });

  describe('startPaymentActivity error handling', () => {
    it('attaches code and status on 409 PAYMENT_NOT_READY rejection', async () => {
      const origFetch = globalThis.fetch;
      globalThis.fetch = (async () => {
        return new Response(
          JSON.stringify({ error: 'bank connection is not in MONITORING state', code: 'PAYMENT_NOT_READY' }),
          { status: 409, headers: { 'Content-Type': 'application/json' } }
        );
      }) as typeof fetch;

      const { startPaymentActivity } = await import('../src/shared/api/queries');
      await expect(startPaymentActivity({ amountVnd: 100000 })).rejects.toMatchObject({
        status: 409,
        code: 'PAYMENT_NOT_READY',
      });

      globalThis.fetch = origFetch;
    });
  });

  describe('Boost rejection vs Generic failure state transition logic', () => {
    function simulateBoostStart({
      isAcbCritical,
      apiError,
    }: {
      isAcbCritical: boolean;
      apiError?: { code?: string; status?: number; message?: string };
    }) {
      let state: 'idle' | 'active' = 'idle';
      let boostDegraded = false;
      let invalidatedReadiness = false;

      // Guard check before calling API
      if (isAcbCritical) {
        return { state, boostDegraded, invalidatedReadiness };
      }

      if (apiError) {
        const isPaymentNotReady =
          apiError.code === 'PAYMENT_NOT_READY' ||
          apiError.status === 409 ||
          (typeof apiError.message === 'string' &&
            (apiError.message.includes('MONITORING') || apiError.message.includes('PAYMENT_NOT_READY')));

        if (isPaymentNotReady) {
          invalidatedReadiness = true;
          state = 'idle';
          boostDegraded = false;
        } else {
          // Generic failure (e.g. RPC unavailable) -> proceed to active with standard polling
          state = 'active';
          boostDegraded = true;
        }
      } else {
        state = 'active';
        boostDegraded = false;
      }

      return { state, boostDegraded, invalidatedReadiness };
    }

    it('stays in IDLE and invalidates readiness when session is dead (PAYMENT_NOT_READY)', () => {
      const outcome = simulateBoostStart({
        isAcbCritical: false,
        apiError: { status: 409, code: 'PAYMENT_NOT_READY', message: 'bank connection is not in MONITORING state' },
      });

      expect(outcome.state).toBe('idle');
      expect(outcome.invalidatedReadiness).toBe(true);
      expect(outcome.boostDegraded).toBe(false);
    });

    it('blocks start immediately when already isAcbCritical', () => {
      const outcome = simulateBoostStart({
        isAcbCritical: true,
      });

      expect(outcome.state).toBe('idle');
      expect(outcome.invalidatedReadiness).toBe(false);
    });

    it('transitions to ACTIVE with degraded status when generic boost RPC fails', () => {
      const outcome = simulateBoostStart({
        isAcbCritical: false,
        apiError: { status: 500, message: 'payment booster unavailable' },
      });

      expect(outcome.state).toBe('active');
      expect(outcome.boostDegraded).toBe(true);
      expect(outcome.invalidatedReadiness).toBe(false);
    });
  });
});
