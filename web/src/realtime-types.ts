import type { PaymentProviderState } from './features/payment-qr/payment-orders';

export type RealtimeStatus = 'CONNECTING' | 'CONNECTED' | 'RECONNECTING' | 'DISCONNECTED';

export type Status = {
  service: string;
  version: string;
  uptimeSeconds: number;
  payments: PaymentProviderState;
  storage: { status: string };
  webhooks: { pending: number; deadLetter: number };
  notifications?: {
    total: { pending: number; deadLetter: number };
    byProvider: Record<string, { pending: number; deadLetter: number }>;
  };
};

export type BarkConfig = {
  group?: string;
  level?: 'passive' | 'active' | 'timeSensitive';
  sound?: string;
  icon?: string;
  includeBalance: boolean;
  includeDescription: boolean;
  dashboardLink: boolean;
};

export type NotificationProvider = {
  id: 'WEBHOOK' | 'BARK';
  name: string;
  description: string;
  configured: boolean;
  publicUrl?: string;
  status?: 'configured' | 'unconfigured' | 'unknown' | 'error' | string;
};

export type NotificationChannel = {
  id: string;
  name: string;
  provider: 'WEBHOOK' | 'BARK';
  status: string;
  revision: number;
  url?: string;
  barkConfig?: BarkConfig;
  hasDeviceKey?: boolean;
  secret?: string;
  createdAt: string;
  updatedAt: string;
};

export type Endpoint = NotificationChannel;

export type Transaction = {
  id: string;
  bank: 'ACB' | 'KienlongBank';
  provider?: 'PAYOS';
  orderCode?: string;
  semanticKey: string;
  transactionDate: string;
  transactionDay?: string;
  datePrecision?: string;
  effectiveDate: string;
  debit: number;
  credit: number;
  balance?: number;
  description: string;
  firstSeenAt: string;
  source?: string;
};

export type Delivery = {
  id: string;
  eventId: string;
  endpointId: string;
  endpointName?: string;
  provider?: string;
  status: string;
  attempts: number;
  nextAttemptAt: string;
  createdAt: string;
  updatedAt: string;
};


export type AuditLog = {
  id: string;
  subject: string;
  role: string;
  action: string;
  target: string;
  createdAt: string;
};

export type TransactionSummary = {
  count: number;
  incoming: number;
  outgoing: number;
};

export type PageResponse<T> = {
  items: T[];
  nextCursor?: string;
  summary?: TransactionSummary;
};

export type RealtimeEvent = {
  id?: string;
  type: string;
  data: unknown;
};

