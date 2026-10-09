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
}

type SlotStorage = Pick<Storage, 'getItem' | 'setItem'>;

const isPaymentOrderSlot = (value: unknown): value is PaymentOrderSlot => {
  if (value === null || typeof value !== 'object') return false;
  const slot = value as Partial<PaymentOrderSlot>;
  return typeof slot.slotId === 'string' && slot.slotId.length > 0 &&
    typeof slot.name === 'string' && typeof slot.idempotencyKey === 'string' &&
    isValidIdempotencyKey(slot.idempotencyKey) && isValidPaymentAmount(slot.amountVnd, Number.MAX_SAFE_INTEGER) &&
    (slot.orderId === undefined || (typeof slot.orderId === 'string' && /^[A-Za-z0-9_-]{43}$/.test(slot.orderId)));
};

const validSlots = (value: unknown): value is PaymentOrderSlot[] => {
  if (!Array.isArray(value) || value.length > MAX_PAYMENT_ORDER_SLOTS || !value.every(isPaymentOrderSlot)) return false;
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

export const loadPaymentOrderSlots = (storage?: SlotStorage): PaymentOrderSlot[] => {
  try {
    const target = storage ?? (typeof window === 'undefined' ? undefined : window.localStorage);
    const raw = target?.getItem(PAYMENT_ORDER_SLOTS_STORAGE_KEY);
    if (!raw) return [];
    const slots: unknown = JSON.parse(raw);
    if (!validSlots(slots)) return [];
    return slots.map(({ slotId, name, idempotencyKey, amountVnd, orderId }) =>
      ({ slotId, name, idempotencyKey, amountVnd, ...(orderId === undefined ? {} : { orderId }) }));
  } catch {
    return [];
  }
};

// Persist the key/amount before POST. Storage failures propagate so the caller
// cannot silently create an unrecoverable payment intent or replace its key.
export const savePaymentOrderSlots = (slots: PaymentOrderSlot[], storage?: SlotStorage): void => {
  if (!validSlots(slots)) throw new Error('Invalid payment order slots');
  const target = storage ?? (typeof window === 'undefined' ? undefined : window.localStorage);
  if (!target) throw new Error('Payment order storage is unavailable');
  const persisted = slots.map(({ slotId, name, idempotencyKey, amountVnd, orderId }) =>
    ({ slotId, name, idempotencyKey, amountVnd, ...(orderId === undefined ? {} : { orderId }) }));
  target.setItem(PAYMENT_ORDER_SLOTS_STORAGE_KEY, JSON.stringify(persisted));
};
