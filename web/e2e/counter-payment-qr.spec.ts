import { expect, test, type Page } from '@playwright/test';
import type { PaymentOrder, PaymentOrderSlot } from '../src/features/payment-qr/payment-orders';

const storageKey = 'payment_order_slots_v1';
const config = { provider: 'PAYOS', bank: 'KienlongBank', staticUrl: 'http://127.0.0.1:5173/pay', minAmountVnd: 1, maxAmountVnd: 500_000_000, ready: true, status: 'READY' };

async function installFixture(page: Page) {
  const orders: PaymentOrder[] = [];
  const creates: Array<{ key: string; persisted: PaymentOrderSlot[]; amountVnd: number }> = [];
  const reads: string[] = [];
  let cancels = 0;
  await page.addInitScript(() => {
    const OriginalEventSource = window.EventSource;
    window.EventSource = new Proxy(OriginalEventSource, {
      construct(target, args: [string | URL, EventSourceInit?]) {
        const source = new target(...args);
        // Test-only property on the real browser window, populated by this fixture.
        const fixtureWindow = window as Window & { __counterSource?: EventSource };
        fixtureWindow.__counterSource = source;
        return source;
      },
    });
  });
  await page.route(/\/api\/public\/v1\/payment-config$/, (route) => route.fulfill({ json: config }));
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
      const persisted = await page.evaluate((key) => JSON.parse(localStorage.getItem(key) ?? '[]'), storageKey) as PaymentOrderSlot[];
      creates.push({ key, persisted, amountVnd: body.amountVnd });
      const order: PaymentOrder = {
        id: String.fromCharCode(65 + orders.length).repeat(43), orderCode: String(100000000001 + orders.length),
        amountVnd: body.amountVnd, origin: 'OPERATOR_DYNAMIC', status: 'PENDING',
        qrCode: `provider-original-payload-${orders.length + 1}`, checkoutUrl: 'https://pay.payos.vn/fixture',
        accountNumber: `VA${orders.length + 1}`, accountName: 'COUNTER FIXTURE', bankBin: '970452',
        expiresAt: new Date(Date.now() + 1_800_000).toISOString(), createdAt: new Date().toISOString(),
      };
      orders.push(order);
      return route.fulfill({ status: 201, json: order });
    }
    const id = url.pathname.split('/').at(-1)!;
    reads.push(id);
    const order = orders.find((item) => item.id === id);
    return route.fulfill({ status: order ? 200 : 404, json: order ?? { error: 'NOT_FOUND' } });
  });
  return { orders, creates, reads, get cancels() { return cancels; } };
}

async function openCounter(page: Page) {
  await page.goto('/transactions');
  await page.getByRole('button', { name: 'Mã QR nhận tiền', exact: true }).click();
  await expect(page.getByRole('dialog', { name: 'Nhận tiền payOS' })).toBeVisible();
}

async function sendCredit(page: Page, order: PaymentOrder) {
  await page.evaluate((order) => {
    // This fixture installs the property while preserving the real EventSource.
    const fixtureWindow = window as Window & { __counterSource?: EventSource };
    const source = fixtureWindow.__counterSource;
    if (!source) throw new Error('Realtime connection not initialized');
    source.dispatchEvent(new MessageEvent('bank.transaction.credit', { data: JSON.stringify({
      bank: 'KienlongBank', provider: 'PAYOS', orderCode: order.orderCode, paymentOrigin: order.origin,
      transactionId: `tx-${order.orderCode}`, transactionNumber: `reference-${order.orderCode}`,
      credit: String(order.amountVnd), debit: '0', currency: 'VND', description: 'Đơn payOS',
      transactionDate: new Date().toISOString(), detectedAt: '2020-01-01T00:00:00Z', source: 'CATCH_UP',
    }) }));
  }, order);
}

test.describe('Independent counter payment orders', () => {
  test('persists before submit, correlates equal amounts out of order and reloads without create', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    for (const [name, amount] of [['Khách 1', '50'], ['Khách 2', '50'], ['Khách 3', '120']]) {
      await page.getByLabel('Tên khách (tùy chọn)').fill(name);
      await page.getByLabel('Số tiền · nghìn đồng').fill(amount);
      await expect(page.getByText(/Xác nhận số tiền:/)).toContainText(`${Number(amount) * 1_000} VND`);
      await page.getByRole('button', { name: /Thêm khách & tạo đơn/ }).click();
      await expect(page.getByRole('article', { name }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
    }
    expect(fixture.creates).toHaveLength(3);
    for (const create of fixture.creates) {
      expect(create.persisted.find((slot) => slot.idempotencyKey === create.key)?.amountVnd).toBe(create.amountVnd);
    }
    expect(new Set(fixture.orders.map((order) => order.orderCode)).size).toBe(3);
    await expect(page.getByRole('button', { name: /Thêm khách & tạo đơn/ })).toBeDisabled();
    // SSE alone never marks PAID, even if the incoming amount matches.
    await sendCredit(page, fixture.orders[1]);
    await expect(page.getByRole('article', { name: 'Khách 2' }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
    for (const index of [1, 0, 2]) {
      fixture.orders[index].status = 'PAID';
      fixture.orders[index].paidAt = new Date().toISOString();
      await sendCredit(page, fixture.orders[index]);
      await expect(page.getByRole('article', { name: `Khách ${index + 1}` }).getByText('Đã thanh toán', { exact: true })).toBeVisible();
      for (const pending of fixture.orders.filter((order) => order.status === 'PENDING')) {
        const pendingIndex = fixture.orders.indexOf(pending);
        await expect(page.getByRole('article', { name: `Khách ${pendingIndex + 1}` }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
      }
    }
    await page.getByRole('button', { name: 'Đóng mã QR' }).click();
    await page.getByRole('button', { name: 'Mã QR nhận tiền', exact: true }).click();
    await expect(page.getByRole('article')).toHaveCount(3);
    await page.reload();
    await page.getByRole('button', { name: 'Mã QR nhận tiền', exact: true }).click();
    await expect(page.getByRole('article')).toHaveCount(3);
    await expect(page.getByText('Đã thanh toán', { exact: true })).toHaveCount(3);
    expect(fixture.creates).toHaveLength(3);
    await page.getByRole('button', { name: 'Bỏ Khách 2 khỏi khay' }).click();
    await expect(page.getByRole('article')).toHaveCount(2);
    expect(fixture.cancels).toBe(0);
  });

  test('rejects invalid thousand amounts and keeps fixed URL QR and WiFi separate', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    for (const input of ['0', '-50', '1.5', '+50', '50k', '9007199254740991']) {
      await page.getByLabel('Số tiền · nghìn đồng').fill(input);
      await expect(page.getByRole('button', { name: /Thêm khách & tạo đơn/ })).toBeDisabled();
    }
    expect(fixture.creates).toHaveLength(0);
    await page.getByRole('button', { name: 'QR cố định', exact: true }).click();
    await expect(page.getByRole('img', { name: 'QR cố định mở trang nhập số tiền' })).toHaveAttribute('src', /^data:image\/png/);
    await expect(page.getByRole('link', { name: config.staticUrl, exact: true })).toHaveAttribute('href', config.staticUrl);
    await expect(page.getByRole('link', { name: 'Tải ảnh QR' })).toHaveAttribute('download', 'payment-url-qr.png');
    await page.getByRole('button', { name: 'WiFi quán', exact: true }).click();
    await page.getByRole('button', { name: 'Sửa thông tin WiFi quán' }).click();
    const dialog = page.getByRole('dialog', { name: 'Nhận tiền payOS' });
    await dialog.getByLabel('Tên WiFi (SSID)', { exact: true }).fill('Counter WiFi');
    await dialog.getByLabel('Mật khẩu WiFi', { exact: true }).fill('secret123');
    await page.getByRole('button', { name: 'Lưu & Tạo mã QR' }).click();
    await expect(page.getByRole('img', { name: 'Mã QR kết nối WiFi' })).toHaveAttribute('src', /^data:image\/png/);
    expect(fixture.creates).toHaveLength(0);
  });

  test('uses DB fallback without SSE and cancels only through an explicit action', async ({ page }) => {
    const fixture = await installFixture(page);
    await openCounter(page);
    await page.getByLabel('Số tiền · nghìn đồng').fill('50');
    await page.getByRole('button', { name: /Thêm khách & tạo đơn/ }).click();
    await expect(page.getByRole('article', { name: 'Khách 1' }).getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
    fixture.orders[0].status = 'PAID';
    await expect(page.getByRole('article', { name: 'Khách 1' }).getByText('Đã thanh toán', { exact: true })).toBeVisible({ timeout: 10_000 });
    await page.getByLabel('Số tiền · nghìn đồng').fill('60');
    await page.getByRole('button', { name: /Thêm khách & tạo đơn/ }).click();
    const card = page.getByRole('article', { name: 'Khách 2' });
    await card.getByRole('button', { name: 'Hủy đơn trên payOS' }).click();
    await expect(card.getByText('Đã hủy', { exact: true })).toBeVisible();
    await expect(card.getByRole('img')).toHaveCount(0);
    expect(fixture.cancels).toBe(1);
  });
  test('public viewer removes cards without a cancel action', async ({ page, baseURL }) => {
    await page.route('https://transactions.tuannguyenviet.site/**', async (route) => {
      const requested = new URL(route.request().url());
      if (requested.pathname.endsWith('/events')) return route.abort();
      const response = await route.fetch({ url: `${baseURL ?? 'http://127.0.0.1:5173'}${requested.pathname}${requested.search}` });
      await route.fulfill({ response });
    });
    const fixture = await installFixture(page);
    await page.goto('https://transactions.tuannguyenviet.site/transactions');
    await page.getByRole('button', { name: 'Mã QR nhận tiền', exact: true }).click();
    await page.getByLabel('Số tiền · nghìn đồng').fill('50');
    await page.getByRole('button', { name: /Thêm khách & tạo đơn/ }).click();
    const card = page.getByRole('article', { name: 'Khách 1' });
    await expect(card.getByText('Đang chờ thanh toán', { exact: true })).toBeVisible();
    await expect(card.getByRole('button', { name: 'Hủy đơn trên payOS' })).toHaveCount(0);
    await card.getByRole('button', { name: 'Bỏ Khách 1 khỏi khay' }).click();
    await expect(page.getByRole('article')).toHaveCount(0);
    expect(fixture.cancels).toBe(0);
    expect(fixture.orders[0].status).toBe('PENDING');
  });

});
