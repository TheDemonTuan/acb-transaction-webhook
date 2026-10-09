import { isValidIdempotencyKey, isValidPaymentAmount } from './payment-orders';

export const GUEST_PAYMENT_INTENT_KEY = 'guest_payment_intent_v1';
export interface GuestPaymentIntent {
  idempotencyKey: string;
  amountVnd: number;
  orderId?: string;
}
type IntentStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;

const validIntent = (value: unknown): value is GuestPaymentIntent => {
  if (!value || typeof value !== 'object') return false;
  const intent = value as Partial<GuestPaymentIntent>;
  return typeof intent.idempotencyKey === 'string' && isValidIdempotencyKey(intent.idempotencyKey) &&
    isValidPaymentAmount(intent.amountVnd, Number.MAX_SAFE_INTEGER) &&
    (intent.orderId === undefined || (typeof intent.orderId === 'string' && /^[A-Za-z0-9_-]{43}$/.test(intent.orderId)));
};

export function loadGuestPaymentIntent(storage: IntentStorage = window.sessionStorage): GuestPaymentIntent | null {
  const raw = storage.getItem(GUEST_PAYMENT_INTENT_KEY);
  if (!raw) return null;
  const value: unknown = JSON.parse(raw);
  if (!validIntent(value)) throw new Error('Invalid saved payment intent');
  return value;
}

export function saveGuestPaymentIntent(intent: GuestPaymentIntent, storage: IntentStorage = window.sessionStorage): void {
  if (!validIntent(intent)) throw new Error('Invalid payment intent');
  storage.setItem(GUEST_PAYMENT_INTENT_KEY, JSON.stringify(intent));
}

export function clearGuestPaymentIntent(storage: IntentStorage = window.sessionStorage): void {
  storage.removeItem(GUEST_PAYMENT_INTENT_KEY);
}

// Redirect admin-host pay links to the configured public site, but keep local dev local.
export function canonicalPaymentRedirect(staticUrl: string, currentUrl: string, isPublic: boolean): string | null {
  const current = new URL(currentUrl);
  if (isPublic || ['localhost', '127.0.0.1', '[::1]'].includes(current.hostname)) return null;
  const canonical = new URL(staticUrl);
  if (canonical.origin === current.origin) return null;
  canonical.pathname = current.pathname;
  canonical.search = current.search;
  canonical.hash = '';
  return canonical.href;
}
