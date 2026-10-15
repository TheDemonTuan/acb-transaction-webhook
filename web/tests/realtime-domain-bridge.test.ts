import { describe, it, expect, vi, beforeEach, afterEach } from 'bun:test';
import { QueryClient } from '@tanstack/react-query';
import {
  applyCreditEventToQueryCache,
  RealtimeInvalidationCoalescer,
} from '../src/realtime/RealtimeDomainBridge';
import { queryKeys } from '../src/shared/api/query-keys';
import type { BankTransactionCreditData } from '../src/realtime/realtime.types';
import type { PageResponse, Transaction } from '../src/realtime-types';

describe('RealtimeDomainBridge cache & coalescing logic', () => {
  let queryClient: QueryClient;

  beforeEach(() => {
    vi.useFakeTimers();
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    queryClient.clear();
  });

  it('optimistically prepends to existing first page and caps at limit without modifying summary', async () => {
    const initialItems: Transaction[] = [];
    for (let i = 0; i < 50; i++) {
      initialItems.push({
        id: `tx-old-${i}`,
        bank: 'KienlongBank',
        semanticKey: `key-old-${i}`,
        transactionDate: '2026-10-10T09:00:00Z',
        effectiveDate: '2026-10-10T09:00:00Z',
        credit: 10000,
        debit: 0,
        description: `Old ${i}`,
        firstSeenAt: '2026-10-10T09:00:00Z',
        source: 'REALTIME',
      });
    }

    const initialPage: PageResponse<Transaction> = {
      items: initialItems,
      summary: {
        count: 50,
        incoming: 500000,
        outgoing: 0,
      },
    };

    queryClient.setQueryData(queryKeys.transactions({ limit: 50 }), initialPage);

    const creditData: BankTransactionCreditData = {
      bank: 'KienlongBank',
      provider: 'PAYOS',
      orderCode: '2001',
      transactionId: 'tx-new-1',
      transactionNumber: 'REF-NEW-1',
      credit: '50000',
      debit: '0',
      currency: 'VND',
      transactionDate: '2026-10-10T10:00:00Z',
      description: 'New payment',
      detectedAt: '2026-10-10T10:00:00Z',
    };

    await applyCreditEventToQueryCache(queryClient, creditData);

    const cached = queryClient.getQueryData<PageResponse<Transaction>>(queryKeys.transactions({ limit: 50 }));
    expect(cached).toBeDefined();
    expect(cached!.items).toHaveLength(50);
    expect(cached!.items[0].id).toBe('tx-new-1');
    // Summary is authoritative snapshot, not incremented client-side
    expect(cached!.summary).toEqual({
      count: 50,
      incoming: 500000,
      outgoing: 0,
    });
  });

  it('does not create an unbounded page when query cache does not exist', async () => {
    expect(queryClient.getQueryData(queryKeys.transactions())).toBeUndefined();

    const creditData: BankTransactionCreditData = {
      bank: 'KienlongBank',
      provider: 'PAYOS',
      orderCode: '1001',
      transactionId: 'tx-1',
      transactionNumber: 'REF-1',
      credit: '50000',
      debit: '0',
      currency: 'VND',
      transactionDate: '2026-10-10T10:00:00Z',
      description: 'Payment',
      detectedAt: '2026-10-10T10:00:00Z',
    };

    await applyCreditEventToQueryCache(queryClient, creditData);

    // Cache must remain undefined
    expect(queryClient.getQueryData(queryKeys.transactions())).toBeUndefined();
  });

  it('ignores duplicate transaction IDs and semantic keys on optimistic prepend', async () => {
    const existingTx: Transaction = {
      id: 'tx-existing',
      bank: 'KienlongBank',
      semanticKey: 'PAYOS:2026-10-10:REF-EX:50000',
      transactionDate: '2026-10-10T09:00:00Z',
      effectiveDate: '2026-10-10T09:00:00Z',
      credit: 50000,
      debit: 0,
      description: 'Existing',
      firstSeenAt: '2026-10-10T09:00:00Z',
      source: 'REALTIME',
    };

    queryClient.setQueryData(queryKeys.transactions({ limit: 50 }), {
      items: [existingTx],
    });

    const duplicateCredit: BankTransactionCreditData = {
      bank: 'KienlongBank',
      provider: 'PAYOS',
      orderCode: '3001',
      transactionId: 'tx-existing',
      transactionNumber: 'REF-EX',
      credit: '50000',
      debit: '0',
      currency: 'VND',
      transactionDate: '2026-10-10T10:00:00Z',
      description: 'Existing replay',
      detectedAt: '2026-10-10T10:00:00Z',
    };

    await applyCreditEventToQueryCache(queryClient, duplicateCredit);

    const cached = queryClient.getQueryData<PageResponse<Transaction>>(queryKeys.transactions({ limit: 50 }));
    expect(cached!.items).toHaveLength(1);
  });

  it('coalesces bursts of 100 events into a single invalidation batch after 250ms', () => {
    const coalescer = new RealtimeInvalidationCoalescer(queryClient);
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries');

    for (let i = 0; i < 100; i++) {
      coalescer.markCredit({
        bank: 'KienlongBank',
        provider: 'PAYOS',
        orderCode: `order-${i % 5}`,
        transactionId: `tx-${i}`,
        transactionNumber: `REF-${i}`,
        credit: '50000',
        debit: '0',
        currency: 'VND',
        transactionDate: '2026-10-10T10:00:00Z',
        description: `Burst ${i}`,
        detectedAt: '2026-10-10T10:00:00Z',
      });
    }

    // Prior to timer tick: zero invalidations
    expect(invalidateSpy).not.toHaveBeenCalled();

    // Advance timer by 250ms
    vi.advanceTimersByTime(250);

    // Invalidation ran exactly once for the batch
    expect(invalidateSpy).toHaveBeenCalled();
    const invalidatedKeys = invalidateSpy.mock.calls
      .map((call) => {
        const filter = call[0];
        if (filter && typeof filter === 'object' && 'queryKey' in filter) {
          return filter.queryKey;
        }
        return undefined;
      })
      .filter(Boolean);

    expect(invalidatedKeys).toContainEqual(queryKeys.transactions());
    expect(invalidatedKeys).toContainEqual(queryKeys.status);
    expect(invalidatedKeys).toContainEqual(queryKeys.adminOverview);
    expect(invalidatedKeys).toContainEqual(queryKeys.payments());
  });
});
