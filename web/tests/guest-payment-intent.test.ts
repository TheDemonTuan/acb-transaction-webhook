import { describe, expect, it } from 'vitest';
import { canonicalPaymentRedirect, clearGuestPaymentIntent, GUEST_PAYMENT_INTENT_KEY, loadGuestPaymentIntent, saveGuestPaymentIntent } from '../src/features/payment-qr/guest-payment-intent';

const key = '3e628a46-7db9-4f7b-a95f-d9763e2af3de';
const id = 'A'.repeat(43);
function storage() {
  const values = new Map<string, string>();
  return {
    getItem: (name: string) => values.get(name) ?? null,
    setItem: (name: string, value: string) => { values.set(name, value); },
    removeItem: (name: string) => { values.delete(name); },
  };
}

describe('guest payment intent recovery', () => {
  it('retains the original amount/key across an unknown POST outcome and binds the returned capability', () => {
    const target = storage();
    const intent = { idempotencyKey: key, amountVnd: 50_000 };
    saveGuestPaymentIntent(intent, target);
    expect(loadGuestPaymentIntent(target)).toEqual(intent);
    saveGuestPaymentIntent({ ...loadGuestPaymentIntent(target)!, orderId: id }, target);
    expect(loadGuestPaymentIntent(target)).toEqual({ ...intent, orderId: id });
    clearGuestPaymentIntent(target);
    expect(loadGuestPaymentIntent(target)).toBeNull();
  });

  it('never silently discards corrupt persisted intent into a new payment', () => {
    const target = storage();
    for (const raw of ['{', JSON.stringify({ idempotencyKey: key, amountVnd: -50 }), JSON.stringify({ idempotencyKey: key, amountVnd: 50, orderId: 'sequential' })]) {
      target.setItem(GUEST_PAYMENT_INTENT_KEY, raw);
      expect(() => loadGuestPaymentIntent(target)).toThrow();
      expect(target.getItem(GUEST_PAYMENT_INTENT_KEY)).toBe(raw);
    }
  });

  it('propagates persistence failure instead of permitting an unpersisted POST intent', () => {
    const target = { ...storage(), setItem: () => { throw new Error('Storage denied'); } };
    expect(() => saveGuestPaymentIntent({ idempotencyKey: key, amountVnd: 50_000 }, target)).toThrow('Storage denied');
  });
});

describe('guest payment canonical origin', () => {
  it('moves admin capability deep links to public origin while preserving return hints only as URL data', () => {
    expect(canonicalPaymentRedirect('https://transactions.example/pay', `https://bank.example/pay/${id}?status=PAID#private`, false))
      .toBe(`https://transactions.example/pay/${id}?status=PAID`);
  });

  it('keeps public, same-origin, and development requests in place', () => {
    expect(canonicalPaymentRedirect('https://transactions.example/pay', 'https://transactions.example/pay', false)).toBeNull();
    expect(canonicalPaymentRedirect('https://transactions.example/pay', 'https://transactions.example/pay', true)).toBeNull();
    for (const host of ['localhost', '127.0.0.1', '[::1]']) {
      expect(canonicalPaymentRedirect('https://transactions.example/pay', `http://${host}:5173/pay`, false)).toBeNull();
    }
  });
});
