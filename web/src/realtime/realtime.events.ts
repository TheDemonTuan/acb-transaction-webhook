import type { BankTransactionCreditData } from './realtime.types';

export const REALTIME_EVENT_TYPES = [
  'bank.transaction.credit',
  'webhook.changed',
  'notification.changed',
  'delivery.changed',
  'audit.created',
  'stream.heartbeat',
  'stream_error',
] as const;

export const KNOWN_REALTIME_TYPES_SET = new Set<string>(REALTIME_EVENT_TYPES);

export function isKnownEventType(type: string): boolean {
  return KNOWN_REALTIME_TYPES_SET.has(type);
}

// Prefer the committed transaction ID; legacy events can fall back to a
// provider-scoped reference without colliding with another bank's history.
export function creditTransactionKey(data: BankTransactionCreditData): string | undefined {
  return data.transactionId ||
    (data.transactionNumber ? `${data.provider ?? data.bank}:${data.transactionNumber}` : undefined);
}
