import { describe, expect, it } from 'vitest';
import {
  archivePaymentOrderSlot,
  attachPaymentOrder,
  creditMatchesPaymentOrder,
  DEFAULT_MAX_AMOUNT_VND,
  isTerminalPaymentOrder,
  isValidIdempotencyKey,
  isValidPaymentAmount,
  loadPaymentOrderTray,
  MAX_PAYMENT_ORDER_SLOTS,
  mergeLegacyPaymentOrderSlots,
  parseCounterAmountVnd,
  parsePaymentAmountVnd,
  PAYMENT_ORDER_SLOTS_STORAGE_KEY,
  PAYMENT_ORDER_TRAY_STORAGE_KEY,
  readPaymentOrderTray,
  savePaymentOrderTray,
  showPaymentOrderSlot,
  type PaymentOrder,
  type PaymentOrderSlot,
  type PaymentOrderTray,
} from '../src/features/payment-qr/payment-orders';

const keys = [
  '3e628a46-7db9-4f7b-a95f-d9763e2af3de',
  '3e628a46-7db9-4f7b-a95f-d9763e2af3df',
  '3e628a46-7db9-4f7b-a95f-d9763e2af3e0',
  '3e628a46-7db9-4f7b-a95f-d9763e2af3e1',
  '3e628a46-7db9-4f7b-a95f-d9763e2af3e2',
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
    expect(orders.map((order) => order.status)).toEqual(['PENDING', 'PENDING', 'PENDING']);
  });

  it('does not correlate SePay credits with payOS orders even with a spoofed matching code', () => {
    const orders = [
      { orderCode: '100000000004', amountVnd: 50_000, status: 'PENDING' },
      { orderCode: '100000000005', amountVnd: 50_000, status: 'PENDING' },
    ];
    for (const order of orders) {
      const credit = {
        provider: 'SEPAY',
        orderCode: order.orderCode,
        credit: '50000',
        transactionId: `txn_sepay_${order.orderCode}`,
      };
      expect(orders.map((candidate) => creditMatchesPaymentOrder(candidate, credit))).toEqual([false, false]);
    }
    expect(orders.map((order) => order.status)).toEqual(['PENDING', 'PENDING']);
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

describe('durable tray and archive persistence boundary', () => {
  it('automatically turnovers visible slots to archive without losing any intent', () => {
    let tray: PaymentOrderTray = { version: 2, visible: [], archived: [] };
    for (let index = 0; index < 5; index += 1) {
      tray = showPaymentOrderSlot(tray, makeSlot(index));
    }
    expect(tray.visible).toHaveLength(MAX_PAYMENT_ORDER_SLOTS);
    expect(tray.visible.map((slot) => slot.slotId)).toEqual(['slot-2', 'slot-3', 'slot-4']);
    expect(tray.archived.map((slot) => slot.slotId)).toEqual(['slot-0', 'slot-1']);
  });

  it('prefers archiving a terminal visible slot first over an unresolved pending intent', () => {
    const slots = [
      { ...makeSlot(0), status: 'PENDING' as const },
      { ...makeSlot(1), status: 'PAID' as const },
      { ...makeSlot(2), status: 'PENDING' as const },
    ];
    let tray: PaymentOrderTray = { version: 2, visible: slots, archived: [] };
    tray = showPaymentOrderSlot(tray, makeSlot(3));
    expect(tray.visible.map((slot) => slot.slotId)).toEqual(['slot-0', 'slot-2', 'slot-3']);
    expect(tray.archived.map((slot) => slot.slotId)).toEqual(['slot-1']);
  });

  it('migrates legacy v1 slots into the new tray without data loss', () => {
    const storage = memoryStorage();
    const v1 = [makeSlot(0), makeSlot(1), makeSlot(2), makeSlot(3)];
    storage.setItem(PAYMENT_ORDER_SLOTS_STORAGE_KEY, JSON.stringify(v1));
    const migrated = readPaymentOrderTray(storage);
    expect(migrated.visible.map((slot) => slot.slotId)).toEqual(['slot-1', 'slot-2', 'slot-3']);
    expect(migrated.archived.map((slot) => slot.slotId)).toEqual(['slot-0']);
  });

  it('attaches order details to an archived entry after late create/snapshot responses', () => {
    let tray: PaymentOrderTray = { version: 2, visible: [makeSlot(1)], archived: [makeSlot(0)] };
    const order: PaymentOrder = {
      id: makeSlot(0).orderId!,
      orderCode: '100000000099',
      amountVnd: 50_000,
      origin: 'OPERATOR_DYNAMIC',
      status: 'PAID',
      expiresAt: new Date().toISOString(),
      createdAt: new Date().toISOString(),
    };
    tray = attachPaymentOrder(tray, makeSlot(0).slotId, order);
    expect(tray.archived[0].orderCode).toBe('100000000099');
    expect(tray.archived[0].status).toBe('PAID');
  });

  it('propagates storage failure so callers never create unpersisted intents', () => {
    const storage = { getItem: () => null, setItem: () => { throw new Error('quota exceeded'); } };
    expect(() => savePaymentOrderTray({ version: 2, visible: [makeSlot(0)], archived: [] }, storage)).toThrow('quota exceeded');
  });

  it('throws on unreadable archive instead of wiping it blindly', () => {
    const storage = memoryStorage();
    storage.setItem(PAYMENT_ORDER_TRAY_STORAGE_KEY, '{corrupted');
    expect(() => readPaymentOrderTray(storage)).toThrow();
    expect(loadPaymentOrderTray(storage)).toEqual({ version: 2, visible: [], archived: [] });
  });

  it('restores manual archive turnover and deduplicates slots', () => {
    const initial: PaymentOrderTray = { version: 2, visible: [makeSlot(0)], archived: [] };
    const archived = archivePaymentOrderSlot(initial, makeSlot(0).slotId);
    expect(archived.visible).toHaveLength(0);
    expect(archived.archived).toHaveLength(1);
    const restored = showPaymentOrderSlot(archived, makeSlot(0));
    expect(restored.visible.map((slot) => slot.slotId)).toEqual(['slot-0']);
    expect(restored.archived).toHaveLength(0);
  });

  it('merges unknown legacy intent keys from an older tab writing to v1 without losing intents', () => {
    const current: PaymentOrderTray = {
      version: 2,
      visible: [makeSlot(0), makeSlot(1)],
      archived: [],
    };
    const legacy = [makeSlot(0), makeSlot(2)];
    const merged = mergeLegacyPaymentOrderSlots(current, legacy);
    expect(merged.visible.map((s) => s.slotId)).toEqual(['slot-0', 'slot-1', 'slot-2']);
  });

  it('attaches late orderId binding from legacy tab to archived slot without resurrecting it as visible', () => {
    const { orderId: _, ...intentWithoutOrder } = makeSlot(0);
    const current: PaymentOrderTray = {
      version: 2,
      visible: [makeSlot(1), makeSlot(2), makeSlot(3)],
      archived: [intentWithoutOrder],
    };
    const legacy = [{ ...intentWithoutOrder, orderId: makeSlot(0).orderId }];
    const merged = mergeLegacyPaymentOrderSlots(current, legacy);
    expect(merged.visible.map((s) => s.slotId)).toEqual(['slot-1', 'slot-2', 'slot-3']);
    expect(merged.archived).toHaveLength(1);
    expect(merged.archived[0].orderId).toBe(makeSlot(0).orderId);
  });

  it('attaches late orderId binding from legacy tab to visible slot', () => {
    const { orderId: _, ...intentWithoutOrder } = makeSlot(0);
    const current: PaymentOrderTray = {
      version: 2,
      visible: [intentWithoutOrder],
      archived: [],
    };
    const legacy = [{ ...intentWithoutOrder, orderId: makeSlot(0).orderId }];
    const merged = mergeLegacyPaymentOrderSlots(current, legacy);
    expect(merged.visible[0].orderId).toBe(makeSlot(0).orderId);
  });

  it('readPaymentOrderTray seamlessly incorporates concurrent legacy tab writes when v2 is present', () => {
    const storage = memoryStorage();
    savePaymentOrderTray({ version: 2, visible: [makeSlot(0)], archived: [] }, storage);
    storage.setItem(PAYMENT_ORDER_SLOTS_STORAGE_KEY, JSON.stringify([makeSlot(0), makeSlot(1)]));
    const loaded = readPaymentOrderTray(storage);
    expect(loaded.visible.map((s) => s.slotId)).toEqual(['slot-0', 'slot-1']);
  });
});
