export type RealtimeStatus = 'CONNECTING' | 'CONNECTED' | 'RECONNECTING' | 'DISCONNECTED';

export type Status = {
  service: string;
  version: string;
  uptimeSeconds: number;
  acb: {
    state: string;
    coverage: string;
    accountMasked?: string;
    generation?: number;
    lastSuccessfulPollAt?: string | null;
    scheduleMode?: PollMode;
    scheduleWindow?: string;
    scheduleMinSeconds?: number;
    scheduleMaxSeconds?: number;
  };
  storage: { status: string };
  webhooks: { pending: number; deadLetter: number };
  notifications?: { pending: number; deadLetter: number };
};

export type Connection = {
  configured: boolean;
  connection?: { id: string; state: string; accountMasked: string; generation: number; updatedAt: string };
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

export type PollMode = 'REALTIME' | 'KEEPALIVE_ONLY' | 'PAUSED';

export type Profile = {
  mode: PollMode;
  minSeconds: number;
  maxSeconds: number;
};

export type Window = {
  name: string;
  daysOfWeek: number[];
  startTime: string;
  endTime: string;
  profile: Profile;
};

export type MonitorSettings = {
  revision: number;
  enabled: boolean;
  timezone: string;
  defaultProfile: Profile;
  windows: Window[];
  updatedAt?: string;
};

export type MonitorSettingsResponse = {
  settings: MonitorSettings;
  current: {
    mode: PollMode;
    minSeconds: number;
    maxSeconds: number;
    activeWindow?: string;
    nextTransitionAt: string;
    nextMode: PollMode;
  };
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

export type PollRun = {
  id: string;
  connectionId: string;
  generation: number;
  status: string;
  classifier?: string;
  httpStatus?: number;
  pages: number;
  rowsSeen: number;
  error?: string;
  startedAt: string;
  finishedAt?: string;
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
