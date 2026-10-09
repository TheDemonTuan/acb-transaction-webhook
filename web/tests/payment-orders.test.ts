import { describe, expect, it } from 'vitest';
import {
  creditMatchesPaymentOrder,
  DEFAULT_MAX_AMOUNT_VND,
  isTerminalPaymentOrder,
  isValidIdempotencyKey,
  isValidPaymentAmount,
  loadPaymentOrderSlots,
  MAX_PAYMENT_ORDER_SLOTS,
  parseCounterAmountVnd,
  parsePaymentAmountVnd,
  PAYMENT_ORDER_SLOTS_STORAGE_KEY,
  savePaymentOrderSlots,
  type PaymentOrderSlot,
} from '../src/features/payment-qr/payment-orders';

const keys = [
  '3e628a46-7db9-4f7b-a95f-d9763e2af3de',
  '3e628a46-7db9-4f7b-a95f-d9763e2af3df',
  '3e628a46-7db9-4f7b-a95f-d9763e2af3e0',
  '3e628a46-7db9-4f7b-a95f-d9763e2af3e1',
];

const memoryStorage = () => {
  const values: Record<string, string> = {};
  return {
    getItem: (key: string) => values[key] ?? null,
    setItem: (key: string, value: string) => { values[key] = value; },
  };
};

const makeSlot = (index: number): PaymentOrderSlot => ({
  slotId: `slot-${index}`,
  name: `Khách ${index + 1}`,
  idempotencyKey: keys[index],
  amountVnd: index === 2 ? 120_000 : 50_000,
  orderId: String.fromCharCode(65 + index).repeat(43),
});

describe('payment amount contracts', () => {
  it('keeps /pay amounts in exact VND, not thousands', () => {
    expect(parsePaymentAmountVnd('50000')).toBe(50_000);
    expect(parsePaymentAmountVnd('50')).toBe(50);
    expect(parsePaymentAmountVnd('00050')).toBe(50);
    expect(parsePaymentAmountVnd('1')).toBe(1);
    expect(parsePaymentAmountVnd(String(DEFAULT_MAX_AMOUNT_VND))).toBe(DEFAULT_MAX_AMOUNT_VND);
    expect(parsePaymentAmountVnd(String(DEFAULT_MAX_AMOUNT_VND + 1))).toBeNull();
  });

  it('converts only strict positive integer counter amounts from thousands', () => {
    expect(parseCounterAmountVnd('50')).toBe(50_000);
    expect(parseCounterAmountVnd('120')).toBe(120_000);
    expect(parseCounterAmountVnd('500000')).toBe(DEFAULT_MAX_AMOUNT_VND);
    expect(parseCounterAmountVnd('500001')).toBeNull();
  });

  it.each(['', '0', '000', '-50', '+50', '1.5', '50.0', '1,000', '50 000', ' 50', '50 ', '1e3', 'NaN', 'Infinity', '５０', '50đ'])
  ('rejects invalid input without stripping it into a positive amount: %s', (input) => {
    expect(parsePaymentAmountVnd(input)).toBeNull();
    expect(parseCounterAmountVnd(input)).toBeNull();
  });

  it('checks safe integers before conversion/multiplication and honors configured caps', () => {
    expect(parsePaymentAmountVnd('9007199254740991', Number.MAX_SAFE_INTEGER)).toBe(Number.MAX_SAFE_INTEGER);
    expect(parsePaymentAmountVnd('9007199254740992', Number.MAX_SAFE_INTEGER)).toBeNull();
    expect(parseCounterAmountVnd('9007199254741', Number.MAX_SAFE_INTEGER)).toBeNull();
    expect(parseCounterAmountVnd('9007199254740', Number.MAX_SAFE_INTEGER)).toBe(9_007_199_254_740_000);
    expect(parsePaymentAmountVnd('100', 99)).toBeNull();
    expect(parseCounterAmountVnd('1', 999)).toBeNull();
    for (const amount of [0, -1, 1.5, '50', NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      expect(isValidPaymentAmount(amount, Number.MAX_SAFE_INTEGER)).toBe(false);
    }
  });

  it('accepts only canonical UUID keys shared with the HTTP contract', () => {
    expect(isValidIdempotencyKey(keys[0])).toBe(true);
    for (const key of ['', keys[0].toUpperCase(), '00000000-0000-0000-0000-000000000000', 'not-a-uuid']) {
      expect(isValidIdempotencyKey(key)).toBe(false);
    }
  });
});

describe('payment credit correlation', () => {
  it('signals only the correct order for equal amounts and out-of-order credits', () => {
    const orders = [
      { orderCode: '100000000001', amountVnd: 50_000, status: 'PENDING' },
      { orderCode: '100000000002', amountVnd: 50_000, status: 'PENDING' },
      { orderCode: '100000000003', amountVnd: 120_000, status: 'PENDING' },
    ];
    for (const index of [1, 0, 2]) {
      const credit = { provider: 'PAYOS', orderCode: orders[index].orderCode };
      expect(orders.map((order) => creditMatchesPaymentOrder(order, credit))).toEqual(
        orders.map((_, candidate) => candidate === index),
      );
    }
    // No mutation: only a committed snapshot can decide PAID.
    expect(orders.map((order) => order.status)).toEqual(['PENDING', 'PENDING', 'PENDING']);
  });

  it('never uses amount, legacy bank credits, missing codes, or numeric coercion', () => {
    const order = { orderCode: '100000000001' };
    expect(creditMatchesPaymentOrder(order, {})).toBe(false);
    expect(creditMatchesPaymentOrder(order, { orderCode: order.orderCode })).toBe(false);
    expect(creditMatchesPaymentOrder(order, { provider: 'ACB', orderCode: order.orderCode })).toBe(false);
    expect(creditMatchesPaymentOrder(order, { provider: 'PAYOS', orderCode: '0100000000001' })).toBe(false);
    expect(creditMatchesPaymentOrder({ orderCode: '' }, { provider: 'PAYOS', orderCode: '' })).toBe(false);
  });

  it('keeps partial and unresolved states nonterminal', () => {
    for (const status of ['CREATING', 'PENDING', 'PROCESSING', 'UNDERPAID'] as const) {
      expect(isTerminalPaymentOrder(status)).toBe(false);
    }
    for (const status of ['PAID', 'CANCELLED', 'EXPIRED', 'FAILED'] as const) {
      expect(isTerminalPaymentOrder(status)).toBe(true);
    }
  });
});

describe('payment slot persistence boundary', () => {
  it('roundtrips three identities, names and original keys without recreating them', () => {
    const storage = memoryStorage();
    const slots = [makeSlot(0), makeSlot(1), makeSlot(2)];
    savePaymentOrderSlots(slots, storage);
    expect(loadPaymentOrderSlots(storage)).toEqual(slots);
    expect(MAX_PAYMENT_ORDER_SLOTS).toBe(3);
  });

  it('persists an intent before the create result and then attaches the returned capability', () => {
    const storage = memoryStorage();
    const { orderId, ...intent } = makeSlot(0);
    savePaymentOrderSlots([intent], storage);
    expect(loadPaymentOrderSlots(storage)).toEqual([intent]);
    savePaymentOrderSlots([{ ...intent, orderId }], storage);
    expect(loadPaymentOrderSlots(storage)[0].idempotencyKey).toBe(intent.idempotencyKey);
    savePaymentOrderSlots([], storage);
    expect(loadPaymentOrderSlots(storage)).toEqual([]);
  });

  it('does not persist QR, checkout, account or stale snapshots', () => {
    const storage = memoryStorage();
    const slot = { ...makeSlot(0), order: { status: 'PAID' }, qrCode: 'private-qr', checkoutUrl: 'private-link', accountNumber: 'private-account' };
    savePaymentOrderSlots([slot], storage);
    expect(JSON.parse(storage.getItem(PAYMENT_ORDER_SLOTS_STORAGE_KEY)!)).toEqual([makeSlot(0)]);
  });

  it('rejects oversize/duplicate slots without overwriting the previous persisted intent', () => {
    const storage = memoryStorage();
    const first = makeSlot(0);
    savePaymentOrderSlots([first], storage);
    for (const invalid of [
      [makeSlot(0), makeSlot(1), makeSlot(2), makeSlot(3)],
      [first, { ...makeSlot(1), slotId: first.slotId }],
      [first, { ...makeSlot(1), idempotencyKey: first.idempotencyKey }],
      [first, { ...makeSlot(1), orderId: first.orderId }],
      [{ ...first, amountVnd: 0 }],
      [{ ...first, orderId: '123' }],
      [{ ...first, idempotencyKey: 'invalid' }],
    ]) {
      expect(() => savePaymentOrderSlots(invalid, storage)).toThrow();
      expect(loadPaymentOrderSlots(storage)).toEqual([first]);
    }
  });

  it('treats corrupted, old, or invalid storage as empty, without trusting saved payment status', () => {
    const storage = memoryStorage();
    for (const invalid of ['{broken', '{}', 'null', JSON.stringify([{ ...makeSlot(0), amountVnd: '50000' }]), JSON.stringify([makeSlot(0), makeSlot(0)]), JSON.stringify([makeSlot(0), makeSlot(1), makeSlot(2), makeSlot(3)])]) {
      storage.setItem(PAYMENT_ORDER_SLOTS_STORAGE_KEY, invalid);
      expect(loadPaymentOrderSlots(storage)).toEqual([]);
    }
    expect(loadPaymentOrderSlots({ getItem: () => { throw new Error('blocked'); }, setItem: () => {} })).toEqual([]);
  });

  it('propagates storage failures so callers cannot send an unpersisted intent', () => {
    const storage = { getItem: () => null, setItem: () => { throw new Error('quota exceeded'); } };
    expect(() => savePaymentOrderSlots([makeSlot(0)], storage)).toThrow('quota exceeded');
  });
});
