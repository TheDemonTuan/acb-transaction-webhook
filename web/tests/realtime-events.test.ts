import { describe, expect, it } from 'vitest';
import { isKnownEventType } from '../src/realtime/realtime.events';

describe('realtime-events', () => {
  it('recognizes authoritative event types', () => {
    expect(isKnownEventType('bank.transaction.credit')).toBe(true);
    expect(isKnownEventType('notification.changed')).toBe(true);
    expect(isKnownEventType('audit.created')).toBe(true);
    expect(isKnownEventType('delivery.changed')).toBe(true);
    expect(isKnownEventType('unknown.random')).toBe(false);
  });

  it('no longer registers ACB poll or authentication events', () => {
    expect(isKnownEventType('poll.completed')).toBe(false);
    expect(isKnownEventType('connection.changed')).toBe(false);
    expect(isKnownEventType('auth.changed')).toBe(false);
  });
});
