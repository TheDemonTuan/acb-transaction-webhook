import { api, ApiError, publicApi } from '../../api';
import {
  isValidIdempotencyKey,
  isValidPaymentAmount,
  type PaymentConfig,
  type PaymentOrder,
  type PaymentOrderStatus,
  type PaymentOrigin,
  type PaymentReview,
} from '../../features/payment-qr/payment-orders';
import type {
  AuditLog,
  BarkConfig,
  Delivery,
  Endpoint,
  NotificationChannel,
  NotificationProvider,
  PageResponse,
  Status,
  Transaction,
} from '../../realtime-types';

export const fetchStatus = async (): Promise<Status> => {
  return api<Status>('/status');
};

export const fetchPaymentConfig = async (): Promise<PaymentConfig> =>
  publicApi<PaymentConfig>('/payment-config');

const paymentCreateInit = (amountVnd: number, idempotencyKey: string, origin?: PaymentOrigin): RequestInit => {
  if (!isValidPaymentAmount(amountVnd, Number.MAX_SAFE_INTEGER)) {
    throw new ApiError('Số tiền phải là số nguyên VND dương.', 400, 'INVALID_AMOUNT');
  }
  if (!isValidIdempotencyKey(idempotencyKey)) {
    throw new ApiError('Khóa tạo đơn không hợp lệ.', 400, 'INVALID_IDEMPOTENCY_KEY');
  }
  return {
    method: 'POST',
    cache: 'no-store',
    headers: { 'Content-Type': 'application/json', 'Idempotency-Key': idempotencyKey },
    body: JSON.stringify({ amountVnd, ...(origin === undefined ? {} : { origin }) }),
  };
};

export const createPublicPaymentOrder = async (
  amountVnd: number,
  origin: PaymentOrigin,
  idempotencyKey: string,
): Promise<PaymentOrder> =>
  publicApi<PaymentOrder>('/payments', paymentCreateInit(amountVnd, idempotencyKey, origin));

export const fetchPublicPaymentOrder = async (id: string): Promise<PaymentOrder> =>
  publicApi<PaymentOrder>(`/payments/${encodeURIComponent(id)}`);

export const createPaymentOrder = async (amountVnd: number, idempotencyKey: string): Promise<PaymentOrder> =>
  api<PaymentOrder>('/payments', paymentCreateInit(amountVnd, idempotencyKey));

export const fetchPaymentOrder = async (id: string): Promise<PaymentOrder> =>
  api<PaymentOrder>(`/payments/${encodeURIComponent(id)}`, { cache: 'no-store' });

export const fetchPaymentOrders = async (params?: {
  status?: PaymentOrderStatus;
  cursor?: string;
  limit?: number;
}): Promise<PageResponse<PaymentOrder>> => {
  const query = new URLSearchParams();
  if (params?.status) query.set('status', params.status);
  if (params?.cursor) query.set('cursor', params.cursor);
  if (params?.limit !== undefined) query.set('limit', String(params.limit));
  const qStr = query.toString();
  return api<PageResponse<PaymentOrder>>(`/payments${qStr ? `?${qStr}` : ''}`, { cache: 'no-store' });
};

export const cancelPaymentOrder = async (id: string): Promise<PaymentOrder> =>
  api<PaymentOrder>(`/payments/${encodeURIComponent(id)}/cancel`, { method: 'POST', cache: 'no-store' });

export const fetchPaymentReviews = async (params?: {
  cursor?: string;
  limit?: number;
}): Promise<PageResponse<PaymentReview>> => {
  const query = new URLSearchParams();
  if (params?.cursor) query.set('cursor', params.cursor);
  if (params?.limit !== undefined) query.set('limit', String(params.limit));
  const qStr = query.toString();
  return api<PageResponse<PaymentReview>>(`/payment-reviews${qStr ? `?${qStr}` : ''}`, { cache: 'no-store' });
};

export const confirmPaymentWebhook = async (): Promise<{ confirmed: true }> =>
  api<{ confirmed: true }>('/payment-provider/confirm-webhook', { method: 'POST', cache: 'no-store' });

export const fetchTransactions = async (params?: {
  from?: string;
  to?: string;
  direction?: 'all' | 'credit' | 'debit';
  query?: string;
  limit?: number;
  cursor?: string;
}): Promise<PageResponse<Transaction>> => {
  const query = new URLSearchParams();
  if (params?.from) query.set('from', params.from);
  if (params?.to) query.set('to', params.to);
  if (params?.direction && params.direction !== 'all') query.set('direction', params.direction);
  if (params?.query) query.set('q', params.query);
  if (params?.limit) query.set('limit', String(params.limit));
  if (params?.cursor) query.set('cursor', params.cursor);
  const qStr = query.toString();
  return api<PageResponse<Transaction>>(`/transactions${qStr ? `?${qStr}` : ''}`);
};

export const fetchTransactionDetail = async (id: string): Promise<Transaction> => {
  return api<Transaction>(`/transactions/${encodeURIComponent(id)}`);
};

export const fetchWebhooks = async (): Promise<{ items: Endpoint[] }> => {
  return api<{ items: Endpoint[] }>('/webhooks');
};

export const fetchDeliveries = async (params?: {
  limit?: number;
  cursor?: string;
}): Promise<PageResponse<Delivery>> => {
  const query = new URLSearchParams();
  if (params?.limit) query.set('limit', String(params.limit));
  if (params?.cursor) query.set('cursor', params.cursor);
  const qStr = query.toString();
  return api<PageResponse<Delivery>>(`/deliveries${qStr ? `?${qStr}` : ''}`);
};

export const fetchAuditLogs = async (params?: {
  limit?: number;
  cursor?: string;
}): Promise<PageResponse<AuditLog>> => {
  const query = new URLSearchParams();
  if (params?.limit) query.set('limit', String(params.limit));
  if (params?.cursor) query.set('cursor', params.cursor);
  const qStr = query.toString();
  return api<PageResponse<AuditLog>>(`/audit${qStr ? `?${qStr}` : ''}`);
};

export const createWebhookEndpoint = async (name: string, url: string): Promise<Endpoint> => {
  return api<Endpoint>('/webhooks', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name, url }),
  });
};

export const toggleWebhookEndpoint = async (
  id: string,
  action: 'enable' | 'disable'
): Promise<Endpoint> => {
  return api<Endpoint>(`/webhooks/${id}/${action}`, {
    method: 'POST',
  });
};

export const fetchNotificationProviders = async (): Promise<{ providers: NotificationProvider[] }> => {
  return api<{ providers: NotificationProvider[] }>('/notification-providers');
};

export const fetchNotificationChannels = async (): Promise<{ items: NotificationChannel[] }> => {
  return api<{ items: NotificationChannel[] }>('/notification-channels');
};

export const createNotificationChannel = async (payload: {
  provider: 'WEBHOOK' | 'BARK';
  name: string;
  url?: string;
  deviceKey?: string;
  barkConfig?: BarkConfig;
}): Promise<NotificationChannel> => {
  return api<NotificationChannel>('/notification-channels', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
};

export const updateNotificationChannel = async (
  id: string,
  payload: {
    expectedRevision: number;
    name: string;
    url?: string;
    barkConfig?: BarkConfig;
  }
): Promise<NotificationChannel> => {
  return api<NotificationChannel>(`/notification-channels/${id}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
};

export const toggleNotificationChannel = async (
  id: string,
  action: 'enable' | 'disable'
): Promise<{ status: string }> => {
  return api<{ status: string }>(`/notification-channels/${id}/${action}`, {
    method: 'POST',
  });
};

export const rotateChannelSecret = async (
  id: string,
  deviceKey?: string
): Promise<{ secret?: string; status: string }> => {
  return api<{ secret?: string; status: string }>(`/notification-channels/${id}/rotate-secret`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ deviceKey }),
  });
};

export const testNotificationChannel = async (
  id: string
): Promise<{ status: string; latencyMs?: number; message?: string }> => {
  return api<{ status: string; latencyMs?: number; message?: string }>(`/notification-channels/${id}/test`, {
    method: 'POST',
  });
};

export const replayDelivery = async (
  id: string
): Promise<{ status: string }> => {
  return api<{ status: string }>(`/deliveries/${id}/replay`, {
    method: 'POST',
  });
};
