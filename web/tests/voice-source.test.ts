import { describe, it, expect } from 'vitest';
import { creditTransactionKey } from '../src/realtime/realtime.events';
import type { BankTransactionCreditData } from '../src/realtime/realtime.types';
import { VoiceDedupe } from '../src/features/voice-announcements/voice-dedupe';

const credit = (changes: Partial<BankTransactionCreditData> = {}): BankTransactionCreditData => ({
  bank: 'KienlongBank',
  provider: 'PAYOS',
  transactionId: 'txn_payos',
  transactionNumber: 'REFERENCE123',
  orderCode: '100000000123',
  credit: '100000',
  debit: '0',
  currency: 'VND',
  transactionDate: '2026-10-08T10:00:00+07:00',
  source: 'REALTIME',
  description: 'Payment',
  detectedAt: new Date().toISOString(),
  ...changes,
});

describe('Voice credit identity', () => {
  it('dedupes replay by committed transaction ID even when event ID changes', () => {
    const dedupe = new VoiceDedupe();
    const original = { eventId: 'epoch1:1', semanticKey: creditTransactionKey(credit()) };
    expect(dedupe.reserve(original)).toBe(true);
    expect(dedupe.isReserved({ eventId: 'epoch2:5', semanticKey: original.semanticKey })).toBe(true);
    dedupe.commit(original);
    expect(dedupe.has({ eventId: 'epoch2:5', semanticKey: original.semanticKey })).toBe(true);
  });

  it('does not merge two committed orders with the same amount or reference', () => {
    const dedupe = new VoiceDedupe();
    dedupe.commit({ semanticKey: creditTransactionKey(credit()) });
    expect(dedupe.has({ semanticKey: creditTransactionKey(credit({ transactionId: 'txn_payos_2' })) })).toBe(false);
  });

  it('scopes reference fallback to the provider and preserves ACB history', () => {
    const payos = creditTransactionKey(credit({ transactionId: '' }));
    const legacy = creditTransactionKey(credit({ transactionId: '', provider: undefined, bank: 'ACB', source: 'CATCH_UP' }));
    expect(payos).toBe('PAYOS:REFERENCE123');
    expect(legacy).toBe('ACB:REFERENCE123');
    const dedupe = new VoiceDedupe();
    dedupe.commit({ semanticKey: legacy });
    expect(dedupe.has({ semanticKey: payos })).toBe(false);
    expect(dedupe.reserve({ semanticKey: payos })).toBe(true);
    dedupe.commit({ semanticKey: payos });
    expect(dedupe.has({ semanticKey: creditTransactionKey(credit({ transactionId: '' })) })).toBe(true);
    expect(creditTransactionKey(credit({ transactionId: '', transactionNumber: '' }))).toBeUndefined();
  });
});
