export const mockStatusHealthy = {
  service: 'HEALTHY',
  version: '2.0.0',
  uptimeSeconds: 3600,
  acb: {
    state: 'MONITORING',
    coverage: 'FULL',
    accountMasked: '***1234',
    generation: 1,
  },
  storage: {
    status: 'READY',
    dbSizeBytes: 5242880,
    walSizeBytes: 1048576,
  },
  webhooks: {
    pending: 0,
    deadLetter: 0,
  },
};

export const mockStatusAuthRequired = {
  ...mockStatusHealthy,
  acb: {
    state: 'AUTH_REQUIRED',
    coverage: 'NOT_STARTED',
    accountMasked: '***1234',
    generation: 1,
  },
};

export const mockStatusUnconfigured = {
  ...mockStatusHealthy,
  acb: {
    state: 'UNCONFIGURED',
    coverage: 'NOT_STARTED',
    accountMasked: '',
    generation: 0,
  },
};

export const mockConnectionConfigured = {
  configured: true,
  connection: {
    id: 'conn_1',
    state: 'MONITORING',
    accountMasked: '***1234',
    generation: 1,
    updatedAt: '2026-09-13T10:00:00Z',
  },
};

export const mockConnectionAuthRequired = {
  configured: true,
  connection: {
    id: 'conn_1',
    state: 'AUTH_REQUIRED',
    accountMasked: '***1234',
    generation: 1,
    updatedAt: '2026-09-13T10:00:00Z',
  },
};

export const mockConnectionUnconfigured = {
  configured: false,
  connection: null,
};

export const mockNotificationProviders = {
  providers: [
    { id: 'BARK', name: 'Bark (iOS Push)', supported: true },
    { id: 'WEBHOOK', name: 'HTTP Webhook', supported: true },
  ],
};

export const mockNotificationChannels = {
  items: [
    {
      id: 'chan_bark_1',
      name: 'Bark Channel',
      provider: 'BARK',
      status: 'ACTIVE',
      destination: 'dev_token_123',
      barkConfig: {
        group: 'ACB',
        level: 'timeSensitive',
        sound: 'shake',
        icon: 'https://api.vietqr.io/img/ACB.png',
        includeBalance: false,
        includeDescription: true,
        dashboardLink: true,
      },
      createdAt: '2026-09-13T08:00:00Z',
    },
    {
      id: 'chan_wh_1',
      name: 'Webhook Production',
      provider: 'WEBHOOK',
      status: 'ACTIVE',
      destination: 'https://webhook.internal.corp/bank',
      createdAt: '2026-09-13T08:30:00Z',
    },
  ],
};

export const mockTransactions = {
  items: [
    {
      id: 'tx_101',
      semanticKey: 'ACB:101',
      transactionDate: '2026-09-13 14:30:00',
      transactionDay: '2026-09-13',
      datePrecision: 'SECOND',
      effectiveDate: '2026-09-13 14:30:00',
      debit: 0,
      credit: 250000,
      description: 'NGUYEN VAN A CHUYEN KHOAN DON HANG 888',
      firstSeenAt: '2026-09-13T14:30:05Z',
      source: 'REALTIME',
    },
    {
      id: 'tx_102',
      semanticKey: 'ACB:102',
      transactionDate: '2026-09-13 11:20:00',
      transactionDay: '2026-09-13',
      datePrecision: 'SECOND',
      effectiveDate: '2026-09-13 11:20:00',
      debit: 50000,
      credit: 0,
      description: 'THANH TOAN TIEN DIEN NUOC',
      firstSeenAt: '2026-09-13T11:20:05Z',
      source: 'POLL',
    },
  ],
  summary: {
    count: 2,
    incoming: 250000,
    outgoing: 50000,
  },
  cursor: null,
};

export const mockPollRuns = {
  items: [
    {
      id: 'poll_1',
      status: 'SUCCESS',
      startedAt: '2026-09-13T14:00:00Z',
      durationMs: 1250,
      transactionsFound: 1,
    },
  ],
};

export const mockDeliveries = {
  items: [
    {
      id: 'del_1',
      channelId: 'chan_wh_1',
      channelName: 'Webhook Production',
      status: 'DELIVERED',
      statusCode: 200,
      attempts: 1,
      createdAt: '2026-09-13T14:30:01Z',
    },
  ],
};

export const mockAuditLogs = {
  items: [
    {
      id: 'audit_1',
      action: 'CHANNEL_CREATE',
      actor: 'admin',
      targetId: 'chan_wh_1',
      createdAt: '2026-09-13T08:30:00Z',
    },
  ],
};

export const mockMonitorSettings = {
  settings: {
    revision: 1,
    enabled: true,
    timezone: 'Asia/Ho_Chi_Minh',
    defaultProfile: {
      mode: 'REALTIME' as const,
      minSeconds: 3,
      maxSeconds: 10,
    },
    windows: [
      {
        name: 'Ban ng??y',
        daysOfWeek: [1, 2, 3, 4, 5, 6, 7],
        startTime: '07:00',
        endTime: '23:00',
        profile: {
          mode: 'REALTIME' as const,
          minSeconds: 3,
          maxSeconds: 10,
        },
      },
      {
        name: 'Ngo??i gi???',
        daysOfWeek: [1, 2, 3, 4, 5, 6, 7],
        startTime: '23:00',
        endTime: '07:00',
        profile: {
          mode: 'KEEPALIVE_ONLY' as const,
          minSeconds: 120,
          maxSeconds: 180,
        },
      },
    ],
  },
  current: {
    mode: 'REALTIME' as const,
    minSeconds: 3,
    maxSeconds: 10,
    activeWindow: 'Ban ng??y',
    nextTransitionAt: '2026-09-13T23:00:00Z',
    nextMode: 'KEEPALIVE_ONLY' as const,
  },
};

export const mockQrSettings = {
  accountNumber: '123456789',
  accountName: 'NGUYEN VIET TUAN',
  bankBin: '970416',
  bankName: 'ACB',
  template: 'compact',
};
