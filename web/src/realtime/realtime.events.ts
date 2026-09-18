export const REALTIME_EVENT_TYPES = [
  'bank.transaction.credit',
  'connection.changed',
  'auth.changed',
  'webhook.changed',
  'notification.changed',
  'delivery.changed',
  'poll.completed',
  'audit.created',
  'stream.heartbeat',
  'stream_error',
] as const;

export const KNOWN_REALTIME_TYPES_SET = new Set<string>(REALTIME_EVENT_TYPES);

export function isKnownEventType(type: string): boolean {
  return KNOWN_REALTIME_TYPES_SET.has(type);
}
