export const queryKeys = {
  status: ['status'] as const,
  transactions: (params?: Record<string, unknown>) =>
    params !== undefined ? (['transactions', params] as const) : (['transactions'] as const),
  transactionDetail: (id: string) => ['transaction', id] as const,
  webhooks: ['webhooks'] as const,
  notificationProviders: ['notification-providers'] as const,
  notificationChannels: ['notification-channels'] as const,
  deliveries: (params?: Record<string, unknown>) =>
    params !== undefined ? (['deliveries', params] as const) : (['deliveries'] as const),
  auditLogs: (params?: Record<string, unknown>) =>
    params !== undefined ? (['auditLogs', params] as const) : (['auditLogs'] as const),
  adminOverview: ['admin-overview'] as const,
  paymentConfig: ['payment-config'] as const,
  payments: (params?: Record<string, unknown>) =>
    params !== undefined ? (['payments', params] as const) : (['payments'] as const),
  paymentOrder: (id: string) => ['payment-order', id] as const,
  publicPaymentOrder: (id: string) => ['public-payment-order', id] as const,
  paymentReviews: (params?: Record<string, unknown>) =>
    params !== undefined ? (['payment-reviews', params] as const) : (['payment-reviews'] as const),
};
