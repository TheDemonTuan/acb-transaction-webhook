import { describe, expect, it, beforeEach } from 'vitest';
import {
  shouldAcceptLiveCredit,
  parseCreditAmount,
  parseDetectedAt,
  type LiveCreditGate,
} from '../src/features/payment-qr/credit-filter';
import type { BankTransactionCreditData } from '../src/realtime/realtime.types';

describe('credit-filter', () => {
  let seenIds: Set<string>;
  const baseNow = 1_700_000_100_000;
  const sessionOpenedAt = 1_700_000_000_000;

  beforeEach(() => {
    seenIds = new Set();
  });

  const makeCredit = (overrides: Partial<BankTransactionCreditData> = {}): BankTransactionCreditData => ({
    bank: 'ACB',
    transactionId: 'tx_123',
    transactionNumber: 'ACB123',
    credit: '500,000',
    debit: '0',
    currency: 'VND',
    transactionDate: '2026-09-17',
    source: 'REALTIME',
    description: 'THANH TOAN DON HANG #101',
    detectedAt: new Date(baseNow).toISOString(),
    ...overrides,
  });

  it('accepts fresh realtime credit within session', () => {
    const gate: LiveCreditGate = { sessionOpenedAt, seenIds, now: baseNow };
    const accepted = shouldAcceptLiveCredit(makeCredit(), gate);
    expect(accepted).toBe(true);
    expect(seenIds.has('tx_123')).toBe(true);
  });

  it('deduplicates identical transaction id', () => {
    const gate: LiveCreditGate = { sessionOpenedAt, seenIds, now: baseNow };
    expect(shouldAcceptLiveCredit(makeCredit(), gate)).toBe(true);
    expect(shouldAcceptLiveCredit(makeCredit(), gate)).toBe(false);
  });

  it('rejects credit with non-positive amount', () => {
    const gate: LiveCreditGate = { sessionOpenedAt, seenIds, now: baseNow };
    expect(shouldAcceptLiveCredit(makeCredit({ credit: '0' }), gate)).toBe(false);
    expect(shouldAcceptLiveCredit(makeCredit({ credit: '-10,000' }), gate)).toBe(false);
    expect(shouldAcceptLiveCredit(makeCredit({ credit: 'invalid' }), gate)).toBe(false);
  });

  it('rejects forbidden sources by default (HISTORY, FILTER_SYNC, BOOTSTRAP, CATCH_UP)', () => {
    const gate: LiveCreditGate = { sessionOpenedAt, seenIds, now: baseNow };
    expect(shouldAcceptLiveCredit(makeCredit({ source: 'HISTORY' }), gate)).toBe(false);
    expect(shouldAcceptLiveCredit(makeCredit({ source: 'FILTER_SYNC' }), gate)).toBe(false);
    expect(shouldAcceptLiveCredit(makeCredit({ source: 'BOOTSTRAP' }), gate)).toBe(false);
    expect(shouldAcceptLiveCredit(makeCredit({ source: 'CATCH_UP' }), gate)).toBe(false);
  });

  it('accepts CATCH_UP only when explicitly enabled in gate sources', () => {
    const gate: LiveCreditGate = {
      sessionOpenedAt,
      seenIds,
      sources: ['REALTIME', 'CATCH_UP'],
      now: baseNow,
    };
    expect(shouldAcceptLiveCredit(makeCredit({ source: 'CATCH_UP' }), gate)).toBe(true);
  });

  it('rejects events older than freshness window', () => {
    const oldDetected = new Date(baseNow - 130_000).toISOString();
    const gate: LiveCreditGate = { sessionOpenedAt: baseNow - 200_000, seenIds, now: baseNow };
    expect(shouldAcceptLiveCredit(makeCredit({ detectedAt: oldDetected }), gate)).toBe(false);
  });

  it('rejects events before sessionOpenedAt minus grace', () => {
    const preSession = new Date(sessionOpenedAt - 10_000).toISOString();
    const gate: LiveCreditGate = { sessionOpenedAt, seenIds, now: baseNow };
    expect(shouldAcceptLiveCredit(makeCredit({ detectedAt: preSession }), gate)).toBe(false);
  });

  it('allows small clock-skew grace (within 5 seconds before sessionOpenedAt)', () => {
    const nearSession = new Date(sessionOpenedAt - 2_000).toISOString();
    const gate: LiveCreditGate = { sessionOpenedAt, seenIds, now: sessionOpenedAt + 1_000 };
    expect(shouldAcceptLiveCredit(makeCredit({ detectedAt: nearSession }), gate)).toBe(true);
  });

  it('parses currency strings cleanly', () => {
    expect(parseCreditAmount('500.000')).toBe(500);
    expect(parseCreditAmount('500,000')).toBe(500000);
    expect(parseCreditAmount('+1.500.000 đ')).toBe(1.5);
    expect(parseCreditAmount('1500000')).toBe(1500000);
    expect(parseCreditAmount(250000)).toBe(250000);
    expect(parseCreditAmount(null)).toBe(0);
  });

  it('handles invalid detectedAt safely', () => {
    const gate: LiveCreditGate = { sessionOpenedAt, seenIds, now: baseNow };
    expect(shouldAcceptLiveCredit(makeCredit({ detectedAt: 'not-a-date' }), gate)).toBe(false);
    expect(shouldAcceptLiveCredit(makeCredit({ detectedAt: '' }), gate)).toBe(false);
    expect(parseDetectedAt('invalid')).toBe(null);
  });
});
