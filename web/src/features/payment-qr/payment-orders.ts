export type PaymentOrigin = 'STATIC_URL' | 'OPERATOR_DYNAMIC';
export type PaymentOrderStatus =
  | 'CREATING'
  | 'PENDING'
  | 'PROCESSING'
  | 'UNDERPAID'
  | 'PAID'
  | 'CANCELLED'
  | 'EXPIRED'
  | 'FAILED';
export type PaymentProviderStatus =
  | 'READY'
  | 'DISABLED'
  | 'UNCONFIGURED'
  | 'WEBHOOK_UNCONFIRMED'
  | 'UNAVAILABLE';

export interface PaymentConfig {
  provider: 'PAYOS';
  bank: 'KienlongBank';
  staticUrl: string;
  minAmountVnd: 1;
  maxAmountVnd: number;
  ready: boolean;
  status: PaymentProviderStatus;
}

// Owner-only safe snapshot. Raw credentials are write-only form values.
export interface PaymentProviderConfig {
  configured: boolean;
  clientId?: string;
  apiKeyConfigured: boolean;
  checksumKeyConfigured: boolean;
  webhookConfirmed: boolean;
  enabled: boolean;
  staticUrl: string;
  webhookUrl: string;
}

export interface PaymentProviderConfigInput {
  clientId: string;
  apiKey: string;
  checksumKey: string;
  enabled: boolean;
}

export interface PaymentProviderState {
  provider: 'PAYOS';
  bank: 'KienlongBank';
  configured: boolean;
  status: PaymentProviderStatus;
  webhookConfirmed: boolean;
  lastWebhookAt: string | null;
  lastReconciledAt: string | null;
  pendingOrders: number;
  reviewCount: number;
}

export interface PaymentOrder {
  id: string;
  orderCode: string;
  amountVnd: number;
  origin: PaymentOrigin;
  status: PaymentOrderStatus;
  qrCode?: string;
  checkoutUrl?: string;
  bankBin?: string;
  accountNumber?: string;
  accountName?: string;
  expiresAt: string;
  createdAt: string;
  paidAt?: string;
  transactionId?: string;
  errorCode?: string;
}

// Admin-only inbox metadata; never include the signed/raw payload or sender details.
export interface PaymentReview {
  payloadHash: string;
  orderCode: string;
  paymentLinkId?: string;
  reference?: string;
  reason: string;
  receivedAt: string;
  processedAt?: string;
}

export const DEFAULT_MAX_AMOUNT_VND = 500_000_000;
export const PAYMENT_ORDER_SLOTS_STORAGE_KEY = 'payment_order_slots_v1';
export const PAYMENT_ORDER_TRAY_STORAGE_KEY = 'payment_order_tray_v2';
export const MAX_PAYMENT_ORDER_SLOTS = 3;

export const isValidPaymentAmount = (
  amountVnd: unknown,
  maxAmountVnd = DEFAULT_MAX_AMOUNT_VND,
): amountVnd is number =>
  typeof amountVnd === 'number' && Number.isSafeInteger(amountVnd) && amountVnd >= 1 &&
  Number.isSafeInteger(maxAmountVnd) && maxAmountVnd >= 1 && amountVnd <= maxAmountVnd;

export const isValidIdempotencyKey = (key: string): boolean =>
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(key);

// Do not strip signs, decimal separators, or other text into a different amount.
export const parsePaymentAmountVnd = (
  input: string,
  maxAmountVnd = DEFAULT_MAX_AMOUNT_VND,
): number | null => {
  if (!/^[0-9]+$/.test(input)) return null;
  const amountVnd = Number(input);
  return isValidPaymentAmount(amountVnd, maxAmountVnd) ? amountVnd : null;
};

// The counter keeps its existing thousands-of-VND convention, unlike /pay.
export const parseCounterAmountVnd = (
  input: string,
  maxAmountVnd = DEFAULT_MAX_AMOUNT_VND,
): number | null => {
  if (!/^[0-9]+$/.test(input)) return null;
  const thousands = Number(input);
  if (!Number.isSafeInteger(thousands) || thousands < 1 ||
      thousands > Math.floor(Number.MAX_SAFE_INTEGER / 1_000)) return null;
  const amountVnd = thousands * 1_000;
  return isValidPaymentAmount(amountVnd, maxAmountVnd) ? amountVnd : null;
};

export const isTerminalPaymentOrder = (status: PaymentOrderStatus): boolean =>
  status === 'PAID' || status === 'CANCELLED' || status === 'EXPIRED' || status === 'FAILED';

// SSE is a refetch signal only; it must never mark a local slot PAID.
export const creditMatchesPaymentOrder = (
  order: Pick<PaymentOrder, 'orderCode'>,
  credit: { provider?: string; orderCode?: string },
): boolean =>
  credit.provider === 'PAYOS' && /^[1-9][0-9]*$/.test(order.orderCode) &&
  credit.orderCode === order.orderCode;

export interface PaymentOrderSlot {
  slotId: string;
  name: string;
  idempotencyKey: string;
  amountVnd: number;
  orderId?: string;
  // Safe recovery metadata only. QR/account/capability snapshots are never cached.
  orderCode?: string;
  status?: PaymentOrderStatus;
}

export interface PaymentOrderTray {
  version: 2;
  visible: PaymentOrderSlot[];
  archived: PaymentOrderSlot[];
}

type SlotStorage = Pick<Storage, 'getItem' | 'setItem'>;
const emptyTray = (): PaymentOrderTray => ({ version: 2, visible: [], archived: [] });
const statuses: PaymentOrderStatus[] = ['CREATING', 'PENDING', 'PROCESSING', 'UNDERPAID', 'PAID', 'CANCELLED', 'EXPIRED', 'FAILED'];

const isPaymentOrderSlot = (value: unknown): value is PaymentOrderSlot => {
  if (value === null || typeof value !== 'object') return false;
  const slot = value as Partial<PaymentOrderSlot>;
  return typeof slot.slotId === 'string' && slot.slotId.length > 0 &&
    typeof slot.name === 'string' && typeof slot.idempotencyKey === 'string' &&
    isValidIdempotencyKey(slot.idempotencyKey) && isValidPaymentAmount(slot.amountVnd, Number.MAX_SAFE_INTEGER) &&
    (slot.orderId === undefined || (typeof slot.orderId === 'string' && /^[A-Za-z0-9_-]{43}$/.test(slot.orderId))) &&
    (slot.orderCode === undefined || /^[1-9][0-9]*$/.test(slot.orderCode)) &&
    (slot.status === undefined || statuses.includes(slot.status));
};

const validSlots = (value: unknown): value is PaymentOrderSlot[] => {
  if (!Array.isArray(value) || !value.every(isPaymentOrderSlot)) return false;
  const ids = new Set<string>();
  const keys = new Set<string>();
  const orders = new Set<string>();
  for (const slot of value) {
    if (ids.has(slot.slotId) || keys.has(slot.idempotencyKey) ||
        (slot.orderId !== undefined && orders.has(slot.orderId))) return false;
    ids.add(slot.slotId);
    keys.add(slot.idempotencyKey);
    if (slot.orderId !== undefined) orders.add(slot.orderId);
  }
  return true;
};

const cleanSlot = ({ slotId, name, idempotencyKey, amountVnd, orderId, orderCode, status }: PaymentOrderSlot): PaymentOrderSlot =>
  ({ slotId, name, idempotencyKey, amountVnd, ...(orderId === undefined ? {} : { orderId }),
    ...(orderCode === undefined ? {} : { orderCode }), ...(status === undefined ? {} : { status }) });
// Throw on unreadable storage rather than overwrite an archive we cannot read.
export const readPaymentOrderTray = (storage?: SlotStorage): PaymentOrderTray => {
  const target = storage ?? (typeof window === 'undefined' ? undefined : window.localStorage);
  if (!target) return emptyTray();
  const raw = target.getItem(PAYMENT_ORDER_TRAY_STORAGE_KEY);
  if (raw !== null) {
    const tray = JSON.parse(raw) as PaymentOrderTray;
    if (tray?.version !== 2 || !Array.isArray(tray.visible) || !Array.isArray(tray.archived) ||
        tray.visible.length > MAX_PAYMENT_ORDER_SLOTS || !validSlots([...tray.visible, ...tray.archived])) {
      throw new Error('Invalid payment order archive');
    }
    const currentTray: PaymentOrderTray = {
      version: 2,
      visible: tray.visible.map(cleanSlot),
      archived: tray.archived.map(cleanSlot),
    };
    const legacyRaw = target.getItem(PAYMENT_ORDER_SLOTS_STORAGE_KEY);
    if (legacyRaw !== null) {
      try {
        const legacySlots: unknown = JSON.parse(legacyRaw);
        if (Array.isArray(legacySlots)) {
          return mergeLegacyPaymentOrderSlots(currentTray, legacySlots.filter(isPaymentOrderSlot));
        }
      } catch { /* keep valid v2 on corrupted legacy */ }
    }
    return currentTray;
  }
  const legacy = target.getItem(PAYMENT_ORDER_SLOTS_STORAGE_KEY);
  if (legacy === null) return emptyTray();
  const slots: unknown = JSON.parse(legacy);
  if (!validSlots(slots)) throw new Error('Invalid payment order slots');
  return { version: 2, visible: slots.slice(-MAX_PAYMENT_ORDER_SLOTS).map(cleanSlot), archived: slots.slice(0, -MAX_PAYMENT_ORDER_SLOTS).map(cleanSlot) };
};

export const loadPaymentOrderTray = (storage?: SlotStorage): PaymentOrderTray => {
  try { return readPaymentOrderTray(storage); } catch { return emptyTray(); }
};

// One atomic setItem contains both sides of turnover. Legacy data is left intact
// until this succeeds; v2 is authoritative thereafter.
export const savePaymentOrderTray = (tray: PaymentOrderTray, storage?: SlotStorage): void => {
  if (tray.version !== 2 || tray.visible.length > MAX_PAYMENT_ORDER_SLOTS || !validSlots([...tray.visible, ...tray.archived])) throw new Error('Invalid payment order tray');
  const target = storage ?? (typeof window === 'undefined' ? undefined : window.localStorage);
  if (!target) throw new Error('Payment order storage is unavailable');
  target.setItem(PAYMENT_ORDER_TRAY_STORAGE_KEY, JSON.stringify({ version: 2, visible: tray.visible.map(cleanSlot), archived: tray.archived.map(cleanSlot) }));
};

export const archivePaymentOrderSlot = (tray: PaymentOrderTray, slotId: string): PaymentOrderTray => {
  const slot = tray.visible.find((item) => item.slotId === slotId);
  return slot ? { version: 2, visible: tray.visible.filter((item) => item.slotId !== slotId), archived: [...tray.archived, slot] } : tray;
};

// Oldest terminal first; otherwise oldest visible intent, even if its POST is
// ambiguous/in flight. Archiving never cancels or forgets the idempotency key.
export const showPaymentOrderSlot = (tray: PaymentOrderTray, slot: PaymentOrderSlot): PaymentOrderTray => {
  if (tray.visible.some((item) => item.slotId === slot.slotId)) return tray;
  let next = { ...tray, archived: tray.archived.filter((item) => item.slotId !== slot.slotId) };
  if (next.visible.length >= MAX_PAYMENT_ORDER_SLOTS) {
    const oldest = next.visible.find((item) => item.status && isTerminalPaymentOrder(item.status)) ?? next.visible[0];
    next = archivePaymentOrderSlot(next, oldest.slotId);
  }
  return { ...next, visible: [...next.visible, slot] };
};

export const attachPaymentOrder = (tray: PaymentOrderTray, slotId: string, order: PaymentOrder): PaymentOrderTray => {
  const attach = (slot: PaymentOrderSlot): PaymentOrderSlot => slot.slotId === slotId ? { ...slot, orderId: order.id, orderCode: order.orderCode, status: order.status } : slot;
  return { version: 2, visible: tray.visible.map(attach), archived: tray.archived.map(attach) };
};

// When an older deployed tab continues writing to payment_order_slots_v1, merge
// any unknown intent keys or late orderId bindings without resurrecting archived slots.
export const mergeLegacyPaymentOrderSlots = (tray: PaymentOrderTray, legacy: PaymentOrderSlot[]): PaymentOrderTray => {
  let next = tray;
  for (const legacySlot of legacy) {
    const visibleIndex = next.visible.findIndex((s) => s.idempotencyKey === legacySlot.idempotencyKey || s.slotId === legacySlot.slotId);
    if (visibleIndex >= 0) {
      const existing = next.visible[visibleIndex];
      if (legacySlot.orderId && !existing.orderId) {
        const updated = {
          ...existing,
          orderId: legacySlot.orderId,
          ...(legacySlot.orderCode ? { orderCode: legacySlot.orderCode } : {}),
          ...(legacySlot.status ? { status: legacySlot.status } : {}),
        };
        next = { ...next, visible: next.visible.map((s, idx) => idx === visibleIndex ? updated : s) };
      }
      continue;
    }
    const archivedIndex = next.archived.findIndex((s) => s.idempotencyKey === legacySlot.idempotencyKey || s.slotId === legacySlot.slotId);
    if (archivedIndex >= 0) {
      const existing = next.archived[archivedIndex];
      if (legacySlot.orderId && !existing.orderId) {
        const updated = {
          ...existing,
          orderId: legacySlot.orderId,
          ...(legacySlot.orderCode ? { orderCode: legacySlot.orderCode } : {}),
          ...(legacySlot.status ? { status: legacySlot.status } : {}),
        };
        next = { ...next, archived: next.archived.map((s, idx) => idx === archivedIndex ? updated : s) };
      }
      continue;
    }
    next = showPaymentOrderSlot(next, legacySlot);
  }
  return next;
};
