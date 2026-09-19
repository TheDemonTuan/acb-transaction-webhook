import { api } from '../../api';
import type {
  AuditLog,
  BarkConfig,
  Connection,
  Delivery,
  Endpoint,
  EnsureHistoryResponse,
  HistorySyncJob,
  NotificationChannel,
  NotificationProvider,
  PageResponse,
  PollRun,
  Status,
  Transaction,
} from '../../realtime-types';

export const fetchStatus = async (): Promise<Status> => {
  return api<Status>('/status');
};

export const fetchConnection = async (): Promise<Connection> => {
  return api<Connection>('/connection');
};

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

export const ensureHistory = async (params: {
  from: string;
  to: string;
}): Promise<EnsureHistoryResponse> => {
  return api<EnsureHistoryResponse>('/transactions/ensure-history', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(params),
  });
};

export const fetchHistorySyncJob = async (id: string): Promise<HistorySyncJob> => {
  return api<HistorySyncJob>(`/transactions/history-sync-jobs/${encodeURIComponent(id)}`);
};

export const fetchLatestHistorySyncJob = async (): Promise<HistorySyncJob | null> => {
  try {
    return await api<HistorySyncJob>('/transactions/history-sync-jobs/latest');
  } catch {
    return null;
  }
};

export const cancelHistorySyncJob = async (id: string): Promise<HistorySyncJob> => {
  return api<HistorySyncJob>(`/transactions/history-sync-jobs/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  });
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

export const fetchPollRuns = async (params?: {
  limit?: number;
  cursor?: string;
}): Promise<PageResponse<PollRun>> => {
  const query = new URLSearchParams();
  if (params?.limit) query.set('limit', String(params.limit));
  if (params?.cursor) query.set('cursor', params.cursor);
  const qStr = query.toString();
  return api<PageResponse<PollRun>>(`/poll-runs${qStr ? `?${qStr}` : ''}`);
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

export const configureConnection = async (accountMasked: string): Promise<Connection> => {
  return api<Connection>('/connection/configure', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ accountMasked }),
  });
};

export const sendConnectionAction = async (action: 'pause' | 'resume' | 'sync'): Promise<{ status: string }> => {
  return api<{ status: string }>(`/connection/${action}`, {
    method: 'POST',
  });
};

export const startAuthSession = async (): Promise<{
  attemptId: string;
  status: string;
  screenUrl: string;
  expiresAt: string;
}> => {
  return api('/connection/auth/start', {
    method: 'POST',
  });
};

export const fetchCurrentAuthSession = async (): Promise<{
  attempt: {
    attemptId: string;
    status: string;
    screenUrl: string;
    expiresAt: string;
    browserUnavailable?: boolean;
  } | null;
}> => {
  return api('/connection/auth/current');
};

export const cancelAuthSession = async (attemptId: string): Promise<void> => {
  await api('/connection/auth/cancel', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ attemptId }),
  });
};

export const fetchMonitorSettings = async (): Promise<any> => {
  return api('/monitor/settings');
};

export const updateMonitorSettings = async (settings: any): Promise<any> => {
  return api('/monitor/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(settings),
  });
};

export const fetchPaymentQR = async (): Promise<any> => {
  return api('/payment-qr');
};

export const uploadPaymentQR = async (formData: FormData): Promise<any> => {
  return api('/payment-qr/upload', {
    method: 'POST',
    body: formData,
  });
};

export const generatePaymentQR = async (params: {
  accountNumber: string;
  accountName: string;
}): Promise<any> => {
  return api('/payment-qr/generate', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(params),
  });
};

export const deletePaymentQR = async (): Promise<any> => {
  return api('/payment-qr', {
    method: 'DELETE',
  });
};

export interface PaymentBoostStatus {
  active: boolean;
  amountVnd: number;
  expiresIn: number;
  phase: number;
  minSeconds: number;
  maxSeconds: number;
}

export const startPaymentActivity = async (payload: { amountVnd: number }): Promise<PaymentBoostStatus> => {
  const res = await fetch('/api/public/v1/payment-activity', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
  if (!res.ok) {
    let msg = `HTTP error ${res.status}`;
    try {
      const data = await res.json();
      if (data?.error) msg = data.error;
    } catch {}
    throw new Error(msg);
  }
  return res.json();
};

export const getDynamicPaymentQRURL = (amountVnd?: number): string => {
  if (amountVnd && amountVnd > 0) {
    return `/api/public/v1/payment-qr/image?amount=${amountVnd}`;
  }
  return '/api/public/v1/payment-qr/image';
};

export const checkAuthStatus = async (attemptId: string): Promise<{
  status: string;
  error?: string;
  generation?: number;
}> => {
  return api(`/connection/auth/${attemptId}/status`);
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
