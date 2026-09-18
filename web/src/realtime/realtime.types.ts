export type RealtimeStatus = 'CONNECTING' | 'CONNECTED' | 'RECONNECTING' | 'DISCONNECTED';

export interface RealtimeEnvelope<T = unknown> {
  id: string | null;
  type: string;
  data: T;
  receivedAt: number;
}

export interface BankTransactionCreditData {
  bank: 'ACB';
  accountMasked?: string;
  transactionId: string;
  transactionNumber: string;
  credit: string;
  debit: string;
  currency: 'VND';
  transactionDate: string;
  transactionDay?: string;
  datePrecision?: string;
  source?: string;
  description: string;
  detectedAt: string;
}

export interface ConnectionChangedData {
  id?: string;
  state: string;
  accountMasked?: string;
  generation?: number;
  updatedAt?: string;
}

export interface AuthChangedData {
  attemptId: string;
  status: string;
  error?: string;
}

export interface WebhookChangedData {
  id?: string;
  status?: string;
  name?: string;
}

export interface NotificationChangedData {
  id?: string;
  status?: string;
  name?: string;
  provider?: string;
}

export interface DeliveryChangedData {
  id: string;
  endpointId: string;
  status: string;
  attempts: number;
}

export interface PollCompletedData {
  id?: string;
  status: string;
  pages?: number;
  rowsSeen?: number;
  insertedCount?: number;
  durationMs?: number;
}

export interface AuditCreatedData {
  id: string;
  action: string;
  subject: string;
  role: string;
}

export interface StreamErrorData {
  code: string;
  message?: string;
}

export interface RealtimeEventMap {
  'bank.transaction.credit': BankTransactionCreditData;
  'connection.changed': ConnectionChangedData;
  'auth.changed': AuthChangedData;
  'webhook.changed': WebhookChangedData;
  'notification.changed': NotificationChangedData;
  'delivery.changed': DeliveryChangedData;
  'poll.completed': PollCompletedData;
  'audit.created': AuditCreatedData;
  'stream_error': StreamErrorData;
}

export type RealtimeEventType = keyof RealtimeEventMap | (string & {});

export type RealtimeListener<T = unknown> = (envelope: RealtimeEnvelope<T>) => void;
