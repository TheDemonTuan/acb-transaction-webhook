export type RealtimeStatus = 'CONNECTING' | 'CONNECTED' | 'STALE' | 'RECONNECTING' | 'DISCONNECTED';

export interface RealtimeEnvelope<T = unknown> {
  id: string | null;
  type: string;
  data: T;
  receivedAt: number;
}

export interface BankTransactionCreditData {
  bank: string;
  provider?: 'PAYOS' | 'SEPAY';
  orderCode?: string;
  paymentOrigin?: 'STATIC_URL' | 'OPERATOR_DYNAMIC';
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



export interface AuditCreatedData {
  id: string;
  action: string;
  subject: string;
  role: string;
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
  'webhook.changed': WebhookChangedData;
  'notification.changed': NotificationChangedData;
  'delivery.changed': DeliveryChangedData;
  'audit.created': AuditCreatedData;
  'stream.heartbeat': StreamHeartbeatData;
  'stream_error': StreamErrorData;
}

export type RealtimeEventType = keyof RealtimeEventMap | (string & {});

export type RealtimeListener<T = unknown> = (envelope: RealtimeEnvelope<T>) => void;
