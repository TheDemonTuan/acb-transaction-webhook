import { expect, test, type Page } from '@playwright/test';
import type { PaymentOrder, PaymentOrderSlot, PaymentOrderTray } from '../src/features/payment-qr/payment-orders';
import type { SePayStoreConfig } from '../src/features/payment-qr/sepay-store';
import type { Transaction } from '../src/realtime-types';

const config = { provider: 'PAYOS', bank: 'KienlongBank', staticUrl: 'http://127.0.0.1:5173/pay', minAmountVnd: 1, maxAmountVnd: 500_000_000, ready: true, status: 'READY' };
const storeConfig: SePayStoreConfig = {
  provider: 'SEPAY', status: 'ACTIVE', storeName: 'Cửa hàng fixture', bank: 'VCB',
  accountNumber: 'STORE000001', accountName: 'STORE FIXTURE', qrPayload: 'fixture-store-bank-qr-payload', lastMessageAt: null,
};

async function readPersistedSlots(page: Page): Promise<PaymentOrderSlot[]> {
  return page.evaluate(() => {
    const rawTray = localStorage.getItem('payment_order_tray_v2');
    if (rawTray) {
      const tray = JSON.parse(rawTray) as PaymentOrderTray;
      return [...tray.visible, ...tray.archived];
    }
    const rawV1 = localStorage.getItem('payment_order_slots_v1');
    return rawV1 ? (JSON.parse(rawV1) as PaymentOrderSlot[]) : [];
  });
}

// Browser-only provider/config/SSE fixtures: not proof of Telegram ingest or bank settlement.
async function installFixture(page: Page) {
  const orders: PaymentOrder[] = [];
  const creates: Array<{ key: string; persisted: PaymentOrderSlot[]; amountVnd: number }> = [];
  const reads: string[] = [];
  const history: Transaction[] = [];
  const byKey = new Map<string, PaymentOrder>();
  let cancels = 0;
  let historyReads = 0;
  let failNextCreate = false;
  let holdNextCreate = false;
  let releaseCreate: (() => void) | undefined;
  const paymentConfig = { ...config };
  const sepayConfig = { ...storeConfig };
  await page.addInitScript(() => {
    const OriginalEventSource = window.EventSource;
    window.EventSource = new Proxy(OriginalEventSource, {
      construct(target, args: [string | URL, EventSourceInit?]) {
        const source = new target(...args);
        const fixtureWindow = window as Window & { __counterSource?: EventSource };
        fixtureWindow.__counterSource = source;
        return source;
      },
    });
  });
  await page.route(/\/api\/public\/v1\/payment-config$/, (route) => route.fulfill({ json: paymentConfig }));
  await page.route(/\/api\/public\/v1\/sepay-store$/, (route) => route.fulfill({ json: sepayConfig }));
  await page.route(/\/api\/(?:public\/)?v1\/transactions(?:\?.*)?$/, (route) => {
    historyReads += 1;
    return route.fulfill({ json: { items: history, summary: { count: history.length, incoming: history.reduce((total, item) => total + Number(item.credit), 0), outgoing: 0 } } });
  });
  await page.route(/\/api\/(?:public\/)?v1\/payments(?:\/[^/?]+)?(?:\/cancel)?$/, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname.endsWith('/cancel')) {
      cancels += 1;
      const order = orders.find((item) => url.pathname.includes(item.id));
      if (!order) return route.fulfill({ status: 404, json: { error: 'NOT_FOUND' } });
      order.status = 'CANCELLED';
      return route.fulfill({ json: order });
    }
    if (request.method() === 'POST') {
      const body = request.postDataJSON() as { amountVnd: number };
      const key = request.headers()['idempotency-key'];
      const persisted = await readPersistedSlots(page);
      creates.push({ key, persisted, amountVnd: body.amountVnd });
      let order = byKey.get(key);
      if (!order) {
        order = {
          id: String.fromCharCode(65 + orders.length).repeat(43), orderCode: String(100000000001 + orders.length),
          amountVnd: body.amountVnd, origin: 'OPERATOR_DYNAMIC', status: 'PENDING',
          qrCode: `provider-original-payload-${orders.length + 1}`, checkoutUrl: 'https://pay.payos.vn/fixture',
          accountNumber: `VA${orders.length + 1}`, accountName: 'COUNTER FIXTURE', bankBin: '970452',
          expiresAt: new Date(Date.now() + 1_800_000).toISOString(), createdAt: new Date().toISOString(),
        };
        orders.push(order);
        byKey.set(key, order);
      }
      if (holdNextCreate) {
        holdNextCreate = false;
        await new Promise<void>((resolve) => { releaseCreate = resolve; });
      }
      if (failNextCreate) {
        failNextCreate = false;
        return route.abort('failed');
      }
      return route.fulfill({ status: 201, json: order });
    }
    const id = url.pathname.split('/').at(-1)!;
    reads.push(id);
    const order = orders.find((item) => item.id === id);
    return route.fulfill({ status: order ? 200 : 404, json: order ?? { error: 'NOT_FOUND' } });
  });
  return {
    orders, creates, reads, history, paymentConfig, sepayConfig,
    get cancels() { return cancels; }, get historyReads() { return historyReads; },
    set failNextCreate(value: boolean) { failNextCreate = value; },
    set holdNextCreate(value: boolean) { holdNextCreate = value; },
    releaseCreate() { releaseCreate?.(); },
  };
}

const counter = (page: Page) => page.getByRole('region', { name: 'Thu ngân', exact: true });
const activeQr = (page: Page) => page.getByTestId('counter-active-qr');

async function openCounter(page: Page) {
  await page.goto('/transactions');
  await expect(counter(page)).toBeVisible();
  await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'store');
}

async function addCustomer(page: Page, name: string, amount: string) {
  if (name) {
    const nameInput = counter(page).getByLabel('Tên khách (tùy chọn)');
    if (!await nameInput.isVisible()) await counter(page).getByText('Thêm tên khách', { exact: true }).click();
    await nameInput.fill(name);
  }
  await counter(page).getByLabel('Số tiền · nghìn đồng').fill(amount);
  await counter(page).getByRole('button', { name: 'Tạo QR payOS', exact: true }).click();
  const card = counter(page).getByRole('article', { name, exact: true });
  await expect(card.getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
  return card;
}

async function sendCredit(page: Page, order: PaymentOrder, provider: 'PAYOS' | 'SEPAY' = 'PAYOS') {
  await page.evaluate(({ order, provider }) => {
    const fixtureWindow = window as Window & { __counterSource?: EventSource };
    const source = fixtureWindow.__counterSource;
    if (!source) throw new Error('Realtime connection not initialized');
    source.dispatchEvent(new MessageEvent('bank.transaction.credit', { data: JSON.stringify({
      bank: provider === 'PAYOS' ? 'KienlongBank' : 'VCB', provider,
      orderCode: order.orderCode, ...(provider === 'PAYOS' ? { paymentOrigin: order.origin } : {}),
      transactionId: `tx-${provider}-${order.orderCode}`, transactionNumber: `reference-${provider}-${order.orderCode}`,
      credit: String(order.amountVnd), debit: '0', currency: 'VND',
      description: provider === 'PAYOS' ? 'Đơn payOS' : 'Thanh toán QR cửa hàng',
      transactionDate: new Date().toISOString(), detectedAt: '2020-01-01T00:00:00Z', source: 'CATCH_UP',
    }) }));
  }, { order, provider });
}

test.describe('Independent fast counter automatic tray turnover and payments', () => {
  test.afterEach(async ({ page }) => {
    await page.unrouteAll({ behavior: 'wait' });
  });

  test('persists before submit, correlates equal amounts out of order and restores selected receipts without create', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    for (const [name, amount] of [['Khách 1', '50'], ['Khách 2', '50'], ['Khách 3', '120']]) {
      await addCustomer(page, name, amount);
    }
    expect(fixture.creates).toHaveLength(3);
    for (const create of fixture.creates) {
      expect(create.persisted.find((slot) => slot.idempotencyKey === create.key)?.amountVnd).toBe(create.amountVnd);
    }
    expect(new Set(fixture.orders.map((order) => order.orderCode)).size).toBe(3);
    // Submit remains enabled because 4th submit auto-archives, not disabled
    await expect(counter(page).getByRole('button', { name: 'Tạo QR payOS', exact: true })).toBeDisabled();
    await counter(page).getByRole('button', { name: 'Hiện QR Khách 1', exact: true }).click();
    const selectedSlot = fixture.creates[0].persisted[0].slotId;
    await expect(activeQr(page)).toHaveAttribute('data-slot-id', selectedSlot);
    const qr = activeQr(page).getByRole('img');
    await expect(qr).toHaveAttribute('src', /^data:image\/png/);
    const originalQr = await qr.getAttribute('src');

    // Matching SSE alone does not authorize PAID
    const pendingSnapshot = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/v1/payments/${fixture.orders[1].id}` && response.request().method() === 'GET');
    await sendCredit(page, fixture.orders[1]);
    expect((await (await pendingSnapshot).json()).status).toBe('PENDING');
    await expect(counter(page).getByRole('article', { name: 'Khách 2', exact: true }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();

    fixture.orders[1].status = 'PAID';
    fixture.orders[1].paidAt = new Date().toISOString();
    await sendCredit(page, fixture.orders[1]);
    await expect(counter(page).getByRole('article', { name: 'Khách 2', exact: true }).getByText('Đã thanh toán', { exact: true })).toBeVisible();
    for (const name of ['Khách 1', 'Khách 3']) {
      await expect(counter(page).getByRole('article', { name, exact: true }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
    }
    await expect(activeQr(page)).toHaveAttribute('data-slot-id', selectedSlot);
    await expect(qr).toHaveAttribute('src', originalQr!);

    for (const index of [0, 2]) {
      fixture.orders[index].status = 'PAID';
      fixture.orders[index].paidAt = new Date().toISOString();
      await sendCredit(page, fixture.orders[index]);
      await expect(counter(page).getByRole('article', { name: `Khách ${index + 1}`, exact: true }).getByText('Đã thanh toán', { exact: true })).toBeVisible();
      for (const pending of fixture.orders.filter((order) => order.status === 'PENDING')) {
        const name = `Khách ${fixture.orders.indexOf(pending) + 1}`;
        await expect(counter(page).getByRole('article', { name, exact: true }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
      }
    }
    await expect(activeQr(page).getByRole('img')).toHaveCount(0);
    await expect(activeQr(page)).toHaveAttribute('data-slot-id', selectedSlot);
    await expect(activeQr(page).getByRole('heading', { name: 'Thanh toán thành công', exact: true })).toBeVisible();

    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(counter(page).getByRole('article')).toHaveCount(3);
    await expect(activeQr(page)).toHaveAttribute('data-slot-id', selectedSlot);
    await expect(activeQr(page).getByRole('heading', { name: 'Thanh toán thành công', exact: true })).toBeVisible();
    expect(fixture.creates).toHaveLength(3);

    const card2 = counter(page).getByRole('article', { name: 'Khách 2', exact: true });
    await card2.getByText('Thao tác đơn', { exact: true }).click();
    await card2.getByRole('button', { name: 'Xong, bỏ khỏi khay', exact: true }).click();
    await expect(counter(page).getByRole('article')).toHaveCount(2);
    expect(fixture.cancels).toBe(0);

    await counter(page).getByRole('button', { name: 'Khách tiếp theo', exact: true }).click();
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'store');
    await expect(counter(page).getByRole('article')).toHaveCount(2);
  });

  test('submitting 5 sequential orders works without manual cleaning and archives older orders reachable in drawer', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);

    // Sequential 5 submissions
    for (let i = 1; i <= 5; i++) {
      const input = counter(page).getByLabel('Số tiền · nghìn đồng');
      await input.fill(String(50 + i * 10));
      await input.press('Enter');
      await expect.poll(() => fixture.creates.length).toBe(i);
    }

    // Only 3 visible in tray at a time
    await expect(counter(page).getByRole('article')).toHaveCount(3);
    const tray = await page.evaluate(() => JSON.parse(localStorage.getItem('payment_order_tray_v2') ?? '{}') as PaymentOrderTray);
    expect(tray.visible).toHaveLength(3);
    expect(tray.archived).toHaveLength(2);

    // Older 2 reachable via drawer
    const drawerBtn = counter(page).getByRole('button', { name: /Đơn trước \(2\)/ });
    await expect(drawerBtn).toBeVisible();
    await drawerBtn.click();

    const archiveDrawer = page.locator('#counter-archive');
    await expect(archiveDrawer).toBeVisible();
    await expect(archiveDrawer.getByRole('article')).toHaveCount(2);

    // Archived item can be inspected
    await archiveDrawer.getByRole('button', { name: 'Xem đơn / QR' }).first().click();
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'payos');

    // Archived item can be restored back to visible tray
    await archiveDrawer.getByRole('button', { name: 'Đưa lại khay' }).first().click();
    await expect(page.locator('[aria-label="Khay khách"]').getByRole('article')).toHaveCount(3);
  });

  test('SePay credit preserves draft, caret, selected QR and pending payOS orders', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    await addCustomer(page, 'Khách 1', '50');
    await addCustomer(page, 'Khách 2', '50');
    const selected = fixture.creates[1].persisted.find((slot) => slot.idempotencyKey === fixture.creates[1].key)!;
    const panel = activeQr(page);
    await expect(panel).toHaveAttribute('data-slot-id', selected.slotId);
    await expect(panel.getByRole('img')).toHaveAttribute('src', /^data:image\/png/);
    const qr = await panel.getByRole('img').getAttribute('src');

    const input = counter(page).getByLabel('Số tiền · nghìn đồng');
    await input.fill('120');
    await input.focus();
    await input.evaluate((element: HTMLInputElement) => element.setSelectionRange(1, 1));

    const date = new Date().toISOString();
    fixture.history.push({
      id: `tx-SEPAY-${fixture.orders[0].orderCode}`, bank: 'VCB', provider: 'SEPAY',
      semanticKey: 'SEPAY:fixture:reference-1', credit: 50_000, debit: 0,
      transactionDate: date, effectiveDate: date, firstSeenAt: date, source: 'CATCH_UP', description: 'Thanh toán QR cửa hàng',
    });
    const before = fixture.historyReads;
    await sendCredit(page, fixture.orders[0], 'SEPAY');
    await expect.poll(() => fixture.historyReads).toBeGreaterThan(before);
    await expect(page.getByText('Thanh toán QR cửa hàng', { exact: true }).first()).toBeVisible();

    await expect(input).toBeFocused();
    await expect(input).toHaveValue('120');
    expect(await input.evaluate((element: HTMLInputElement) => [element.selectionStart, element.selectionEnd])).toEqual([1, 1]);
    await expect(panel).toHaveAttribute('data-slot-id', selected.slotId);
    await expect(panel.getByRole('img')).toHaveAttribute('src', qr!);
    for (const name of ['Khách 1', 'Khách 2']) {
      await expect(counter(page).getByRole('article', { name, exact: true }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
    }
    expect(fixture.creates).toHaveLength(2);
    expect(fixture.orders.every((order) => order.status === 'PENDING')).toBe(true);
  });

  test('Enter persists one intent, selects the creating slot, and a late response does not switch away from Store', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    fixture.holdNextCreate = true;
    const input = counter(page).getByLabel('Số tiền · nghìn đồng');
    await input.fill('50');
    await input.press('Enter');
    await expect.poll(() => fixture.creates.length).toBe(1);
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'payos');
    await expect(activeQr(page).getByRole('img')).toHaveCount(0);
    await expect(input).toHaveValue('');
    await input.press('Enter');
    expect(fixture.creates).toHaveLength(1);

    await counter(page).getByRole('button', { name: 'QR cửa hàng · SePay', exact: true }).click();
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'store');
    fixture.releaseCreate();
    await expect(counter(page).getByRole('article', { name: 'Khách 1', exact: true }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'store');
    expect(fixture.creates).toHaveLength(1);
  });

  test('a creation response after navigation cannot erase a newer persisted customer', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    fixture.holdNextCreate = true;
    await counter(page).getByLabel('Số tiền · nghìn đồng').fill('50');
    await counter(page).getByLabel('Số tiền · nghìn đồng').press('Enter');
    await expect.poll(() => fixture.creates.length).toBe(1);

    await page.getByRole('button', { name: 'Quản trị', exact: true }).click();
    await expect(counter(page)).toHaveCount(0);
    await page.getByRole('button', { name: 'Giao dịch', exact: true }).first().click();
    await expect(counter(page)).toBeVisible();

    await counter(page).getByLabel('Số tiền · nghìn đồng').fill('120');
    await counter(page).getByLabel('Số tiền · nghìn đồng').press('Enter');
    await expect.poll(() => fixture.creates.length).toBe(2);
    await expect(counter(page).getByRole('article', { name: 'Khách 2', exact: true }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();

    const firstResponse = page.waitForResponse(response => response.request().method() === 'POST' && response.request().headers()['idempotency-key'] === fixture.creates[0].key);
    fixture.releaseCreate();
    await firstResponse;

    await page.reload();
    await expect(counter(page).getByRole('article')).toHaveCount(2);
    const persisted = await readPersistedSlots(page);
    expect(persisted.map(slot => slot.idempotencyKey)).toEqual(fixture.creates.map(create => create.key));
    expect(persisted.map(slot => slot.amountVnd)).toEqual([50_000, 120_000]);

    expect(persisted[0].orderId).toBe(fixture.orders[0].id);
    expect(fixture.creates).toHaveLength(2);
  });

  test('uncertain creation recovers the persisted amount and key after reload', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    fixture.failNextCreate = true;
    await counter(page).getByLabel('Số tiền · nghìn đồng').fill('50');
    await counter(page).getByRole('button', { name: 'Tạo QR payOS', exact: true }).click();
    await expect.poll(() => fixture.creates.length).toBe(1);
    await expect(counter(page).getByRole('button', { name: 'Thử lại cùng đơn', exact: true })).toBeEnabled();

    const original = fixture.creates[0];
    await page.reload();
    await expect(counter(page).getByRole('article')).toHaveCount(1);
    fixture.holdNextCreate = true;
    await counter(page).getByRole('button', { name: 'Thử lại cùng đơn', exact: true }).click();
    await expect.poll(() => fixture.creates.length).toBe(2);
    await expect(counter(page).getByRole('button', { name: 'Thử lại cùng đơn', exact: true })).toBeDisabled();
    await expect(activeQr(page).getByRole('img')).toHaveCount(0);

    fixture.releaseCreate();
    await expect(counter(page).getByRole('article', { name: 'Khách 1', exact: true }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
    expect(fixture.creates).toHaveLength(2);
    expect(fixture.creates[1].key).toBe(original.key);
    expect(fixture.creates[1].amountVnd).toBe(50_000);
    expect(fixture.orders).toHaveLength(1);
  });

  test('rejects invalid thousand amounts and keeps payment-link QR and WiFi in utilities', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    for (const input of ['0', '-50', '1.5', '+50', '50k', '9007199254740991']) {
      await counter(page).getByLabel('Số tiền · nghìn đồng').fill(input);
      await expect(counter(page).getByRole('button', { name: 'Tạo QR payOS', exact: true })).toBeDisabled();
    }
    expect(fixture.creates).toHaveLength(0);

    await counter(page).getByText('Tiện ích', { exact: true }).click();
    await expect(counter(page).getByRole('link', { name: config.staticUrl, exact: true })).toHaveAttribute('href', config.staticUrl);
    await expect(counter(page).getByRole('link', { name: 'Tải ảnh QR', exact: true })).toHaveAttribute('download', 'payment-url-qr.png');
    await counter(page).getByRole('button', { name: 'Sửa thông tin WiFi', exact: true }).click();
    await counter(page).getByLabel('Tên WiFi (SSID)', { exact: true }).fill('Counter WiFi');
    await counter(page).getByLabel('Mật khẩu WiFi', { exact: true }).fill('secret123');
    await counter(page).getByRole('button', { name: 'Lưu WiFi', exact: true }).click();
    await expect(counter(page).getByRole('img', { name: 'Mã QR kết nối WiFi', exact: true })).toHaveAttribute('src', /^data:image\/png/);
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'store');
    expect(fixture.creates).toHaveLength(0);
  });

  test('enlargement traps focus, preserves selection on credit, and restores opener on Escape', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    await addCustomer(page, 'Khách 1', '50');
    const opener = counter(page).getByRole('button', { name: 'Phóng to QR', exact: true });
    await opener.click();
    const dialog = page.getByRole('dialog', { name: 'Phóng to QR', exact: true });
    await expect(dialog.getByRole('img')).toHaveAttribute('src', /^data:image\/png/);
    await page.keyboard.press('Tab');
    expect(await dialog.evaluate((element) => element.contains(document.activeElement))).toBe(true);
    await page.keyboard.press('Shift+Tab');
    expect(await dialog.evaluate((element) => element.contains(document.activeElement))).toBe(true);
    await sendCredit(page, fixture.orders[0], 'SEPAY');
    await expect(dialog.getByRole('img')).toBeVisible();
    fixture.orders[0].status = 'PAID';
    await sendCredit(page, fixture.orders[0]);
    await expect(dialog.getByRole('img')).toHaveCount(0);
    await expect(dialog.getByRole('heading', { name: 'Thanh toán thành công', exact: true })).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(dialog).toHaveCount(0);
    await expect(opener).toBeFocused();
  });

  test('uses DB fallback without SSE and cancels only through an explicit private action', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    const first = await addCustomer(page, 'Khách 1', '50');
    fixture.orders[0].status = 'PAID';
    await expect(first.getByText('Đã thanh toán', { exact: true })).toBeVisible({ timeout: 10_000 });
    const card = await addCustomer(page, 'Khách 2', '60');
    await card.getByText('Thao tác đơn', { exact: true }).click();
    await card.getByRole('button', { name: 'Hủy đơn trên payOS', exact: true }).click();
    await expect(card.getByText('Đã hủy', { exact: true })).toBeVisible();
    await expect(activeQr(page).getByRole('img')).toHaveCount(0);
    expect(fixture.cancels).toBe(1);
  });

  test('public cashier removes pending cards only after confirmation and never cancels orders', async ({ page, baseURL }) => {
    await page.route('https://transactions.tuannguyenviet.site/**', async (route) => {
      const requested = new URL(route.request().url());
      if (requested.pathname.endsWith('/events')) return route.abort();
      const response = await route.fetch({ url: `${baseURL ?? 'http://127.0.0.1:5173'}${requested.pathname}${requested.search}` });
      await route.fulfill({ response });
    });
    const fixture = await installFixture(page);
    await page.goto('https://transactions.tuannguyenviet.site/');
    await expect(counter(page)).toBeVisible();
    const card = await addCustomer(page, 'Khách 1', '50');
    await card.getByText('Thao tác đơn', { exact: true }).click();
    await expect(card.getByRole('button', { name: 'Hủy đơn trên payOS', exact: true })).toHaveCount(0);
    await card.getByRole('button', { name: 'Bỏ khỏi khay', exact: true }).click();
    await expect(card).toBeVisible();
    await card.getByRole('button', { name: 'Giữ lại', exact: true }).click();
    await expect(card).toBeVisible();
    await card.getByRole('button', { name: 'Bỏ khỏi khay', exact: true }).click();
    await card.getByRole('button', { name: 'Xác nhận bỏ khỏi khay', exact: true }).click();
    await expect(counter(page).getByRole('article')).toHaveCount(0);
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'store');
    expect(fixture.cancels).toBe(0);
    expect(fixture.orders[0].status).toBe('PENDING');
  });

  test('payOS unavailable leaves Store QR usable without creating an order', async ({ page }) => {
    const fixture = await installFixture(page);
    fixture.paymentConfig.ready = false;
    fixture.paymentConfig.status = 'DISABLED';
    await openCounter(page);
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'store');
    await counter(page).getByLabel('Số tiền · nghìn đồng').fill('50');
    await expect(counter(page).getByRole('button', { name: 'Tạo QR payOS', exact: true })).toBeDisabled();
    expect(fixture.creates).toHaveLength(0);
  });

  test('Store unavailable does not block an explicit payOS order', async ({ page }) => {
    const fixture = await installFixture(page);
    Object.assign(fixture.sepayConfig, { status: 'DISABLED', storeName: '', bank: '', accountNumber: '', accountName: '', qrPayload: '' });
    await openCounter(page);
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'store');
    expect(fixture.creates).toHaveLength(0);
    await addCustomer(page, 'Khách 1', '50');
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'payos');
    await expect(activeQr(page).getByRole('img')).toHaveAttribute('src', /^data:image\/png/);
    expect(fixture.creates).toHaveLength(1);
  });
  test('legacy open tab writing payment_order_slots_v1 merges intent and missing order bindings into v2 without resurrecting archived', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    for (let i = 1; i <= 4; i++) {
      const input = counter(page).getByLabel('Số tiền · nghìn đồng');
      await input.fill(String(50 + i * 10));
      await input.press('Enter');
      await expect.poll(() => fixture.creates.length).toBe(i);
    }
    // Slot 1 is archived, slots 2, 3, 4 are visible
    const slot1Key = fixture.creates[0].key;
    const legacyExtraSlot: PaymentOrderSlot = {
      slotId: 'legacy-extra-slot-id',
      name: 'Khách Legacy Tab',
      idempotencyKey: '00000000-1111-4000-8000-000000000001',
      amountVnd: 75_000,
    };
    await page.evaluate(({ slot1Key, legacyExtraSlot, order1Id }) => {
      const rawTray = JSON.parse(localStorage.getItem('payment_order_tray_v2') ?? '{}') as PaymentOrderTray;
      const slot1 = rawTray.archived.find((s) => s.idempotencyKey === slot1Key);
      const legacySlots: PaymentOrderSlot[] = [
        { ...slot1!, orderId: order1Id },
        legacyExtraSlot,
      ];
      localStorage.setItem('payment_order_slots_v1', JSON.stringify(legacySlots));
      window.dispatchEvent(new StorageEvent('storage', { key: 'payment_order_slots_v1' }));
    }, { slot1Key, legacyExtraSlot, order1Id: fixture.orders[0].id });

    // The archived slot must still be archived (not resurrected as visible), but with its orderId attached
    await expect.poll(async () => {
      const tray = await page.evaluate(() => JSON.parse(localStorage.getItem('payment_order_tray_v2') ?? '{}') as PaymentOrderTray);
      return tray.archived?.find((s) => s.idempotencyKey === slot1Key)?.orderId === fixture.orders[0].id &&
        [...tray.visible, ...tray.archived].some((s) => s.idempotencyKey === legacyExtraSlot.idempotencyKey);
    }).toBe(true);

    const finalTray = await page.evaluate(() => JSON.parse(localStorage.getItem('payment_order_tray_v2') ?? '{}') as PaymentOrderTray);
    expect(finalTray.visible.some((s) => s.idempotencyKey === slot1Key)).toBe(false);
    // The legacy extra slot was merged without loss
    expect([...finalTray.visible, ...finalTray.archived].some((s) => s.idempotencyKey === legacyExtraSlot.idempotencyKey)).toBe(true);
  });

  test('late create binds the remounted cashier without reload or another POST', async ({ page }) => {
    const fixture = await installFixture(page);
    fixture.holdNextCreate = true;
    await openCounter(page);
    await counter(page).getByLabel('Số tiền · nghìn đồng').fill('50');
    await expect(counter(page).getByRole('button', { name: 'Tạo QR payOS', exact: true })).toBeEnabled();
    await counter(page).getByLabel('Số tiền · nghìn đồng').press('Enter');
    await expect.poll(() => fixture.creates.length).toBe(1);
    await page.getByRole('button', { name: 'Tổng quan', exact: true }).click();
    await expect(page).toHaveURL(/\/admin(?:\/overview)?$/);
    await page.getByRole('button', { name: 'Giao dịch', exact: true }).click();
    const card = counter(page).getByRole('article', { name: 'Khách 1', exact: true });
    await expect(card).toBeVisible();
    fixture.orders[0].status = 'PAID';
    fixture.releaseCreate();
    await expect(card.getByText('Đã thanh toán', { exact: true })).toBeVisible();
    await expect(activeQr(page).getByRole('heading', { name: 'Thanh toán thành công', exact: true })).toBeVisible();
    expect(fixture.creates).toHaveLength(1);
  });

  test('queued persistence preserves newer same-valued draft, Store selection and focus', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    const amount = counter(page).getByLabel('Số tiền · nghìn đồng');
    await amount.fill('50');
    await expect(counter(page).getByRole('button', { name: 'Tạo QR payOS', exact: true })).toBeEnabled();
    await page.evaluate(() => {
      const fixtureWindow = window as Window & { __releaseTrayLock?: () => void };
      void navigator.locks.request('payment_order_tray_v2', () => new Promise<void>((resolve) => {
        fixtureWindow.__releaseTrayLock = resolve;
      }));
    });
    await expect.poll(() => page.evaluate(async () => (await navigator.locks.query()).held?.some((lock) => lock.name === 'payment_order_tray_v2'))).toBe(true);
    await amount.press('Enter');
    await expect.poll(() => page.evaluate(async () => (await navigator.locks.query()).pending?.some((lock) => lock.name === 'payment_order_tray_v2'))).toBe(true);
    await amount.fill('');
    await amount.fill('50');
    await counter(page).getByRole('button', { name: 'QR cửa hàng · SePay', exact: true }).click();
    const nameInput = counter(page).getByLabel('Tên khách (tùy chọn)');
    if (!await nameInput.isVisible()) await counter(page).getByText('Thêm tên khách', { exact: true }).click();
    await nameInput.fill('Khách tiếp theo');
    await page.evaluate(() => (window as Window & { __releaseTrayLock?: () => void }).__releaseTrayLock?.());
    await expect(counter(page).getByRole('article', { name: 'Khách 1', exact: true }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
    await expect(amount).toHaveValue('50');
    await expect(nameInput).toHaveValue('Khách tiếp theo');
    await expect(nameInput).toBeFocused();
    await expect(activeQr(page)).toHaveAttribute('data-qr-kind', 'store');
    expect(fixture.creates).toHaveLength(1);
    expect(fixture.creates[0].amountVnd).toBe(50_000);
  });

});
