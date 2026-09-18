export const queryKeys = {
  status: ['status'] as const,
  connection: ['connection'] as const,
  transactions: (params?: Record<string, unknown>) =>
    params !== undefined ? (['transactions', params] as const) : (['transactions'] as const),
  transactionDetail: (id: string) => ['transaction', id] as const,
  webhooks: ['webhooks'] as const,
  notificationProviders: ['notification-providers'] as const,
  notificationChannels: ['notification-channels'] as const,
  deliveries: (params?: Record<string, unknown>) =>
    params !== undefined ? (['deliveries', params] as const) : (['deliveries'] as const),
  pollRuns: (params?: Record<string, unknown>) =>
    params !== undefined ? (['pollRuns', params] as const) : (['pollRuns'] as const),
  auditLogs: (params?: Record<string, unknown>) =>
    params !== undefined ? (['auditLogs', params] as const) : (['auditLogs'] as const),
  adminOverview: ['admin-overview'] as const,
  monitorSettings: ['monitor-settings'] as const,
  paymentQR: ['payment-qr'] as const,
  publicPaymentQR: ['public-payment-qr'] as const,
  historySyncJob: (id: string) => ['history-sync-job', id] as const,
  latestHistorySyncJob: () => ['history-sync-job', 'latest'] as const,
};
