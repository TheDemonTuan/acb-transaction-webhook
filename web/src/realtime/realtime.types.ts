export type RealtimeStatus = 'CONNECTING' | 'CONNECTED' | 'STALE' | 'RECONNECTING' | 'DISCONNECTED';

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
  pollId?: string;
  status: string;
  classifier?: string;
  httpStatus?: number;
  pages?: number;
  rowsSeen?: number;
  insertedCount?: number;
  durationMs?: number;
  error?: string;
  startedAt?: string;
  finishedAt?: string;
}

export interface AuditCreatedData {
  id: string;
  action: string;
  subject: string;
  role: string;
}

export interface PaymentActivatedData {
  id?: string;
  source?: string;
  timestamp?: number | string;
  mode?: string;
  rate?: string | number;
}

export interface StreamHeartbeatData {
  serverTime: string;
  epoch: string;
  release?: string;
  slot?: string;
}

export interface StreamErrorData {
  reason?: string;
  code?: string;
  message?: string;
}

export interface RealtimeDiagnostics {
  status: RealtimeStatus;
  lastHeartbeatAt: number | null;
  lastMessageAt: number | null;
  lastEventId: string | null;
  connectedAt: number | null;
  disconnectedAt: number | null;
  reconnectCount: number;
  lastError: unknown | null;
  networkOnline: boolean;
  serverReachable: boolean;
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
  'payment.activated': PaymentActivatedData;
  'stream.heartbeat': StreamHeartbeatData;
  'stream_error': StreamErrorData;
}

export type RealtimeEventType = keyof RealtimeEventMap | (string & {});

export type RealtimeListener<T = unknown> = (envelope: RealtimeEnvelope<T>) => void;
