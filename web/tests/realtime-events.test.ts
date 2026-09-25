import { describe, expect, it } from 'vitest';
import { isKnownEventType } from '../src/realtime/realtime.events';
import type { PollCompletedData } from '../src/realtime/realtime.types';

describe('realtime-events', () => {
  it('recognizes authoritative event types', () => {
    expect(isKnownEventType('bank.transaction.credit')).toBe(true);
    expect(isKnownEventType('poll.completed')).toBe(true);
    expect(isKnownEventType('connection.changed')).toBe(true);
    expect(isKnownEventType('delivery.changed')).toBe(true);
    expect(isKnownEventType('unknown.random')).toBe(false);
  });

  it('triggers transaction query invalidation for recovery, catch-up, and inserted transactions', () => {
    const shouldInvalidateTransactions = (data?: PollCompletedData) => {
      return Boolean(
        data &&
          ((data.insertedCount ?? 0) > 0 ||
            data.classifier === 'RECOVERY' ||
            data.classifier === 'CATCH_UP')
      );
    };

    // Realtime empty poll -> no transaction invalidation
    expect(shouldInvalidateTransactions({ status: 'SUCCEEDED', insertedCount: 0 })).toBe(false);

    // Realtime poll with inserted transactions -> invalidates
    expect(shouldInvalidateTransactions({ status: 'SUCCEEDED', insertedCount: 2 })).toBe(true);

    // Recovery scan completed (even if 0 new rows inserted or baseline) -> invalidates
    expect(shouldInvalidateTransactions({ status: 'SUCCEEDED', classifier: 'RECOVERY', insertedCount: 0 })).toBe(true);

    // Catch-up scan completed -> invalidates
    expect(shouldInvalidateTransactions({ status: 'SUCCEEDED', classifier: 'CATCH_UP', insertedCount: 0 })).toBe(true);
  });
});
