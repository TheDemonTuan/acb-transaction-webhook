import { expect, test } from '@playwright/test';
import type { PaymentOrder } from '../src/features/payment-qr/payment-orders';

const id = 'A'.repeat(43);
const orderCode = '123456789012';

// Browser-only UI contract fixtures, not proof of provider settlement or a payOS sandbox.
test.describe('guest payment pages', () => {
  let order: PaymentOrder;
  let creates: Array<{ key: string | undefined; amountVnd: number }>;
  let getCount: number;
  let failNextCreate: boolean;
  let requests: string[];

  test.beforeEach(async ({ page, baseURL }) => {
    order = {
      id, orderCode, amountVnd: 50_000, origin: 'STATIC_URL', status: 'PENDING',
      qrCode: 'payOS-original-QR-payload-for-this-order',
      checkoutUrl: `https://pay.payos.vn/web/${'b'.repeat(32)}`,
      accountNumber: 'VA000001', accountName: 'CUA HANG', bankBin: '970452',
      createdAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 30 * 60_000).toISOString(),
    };
    creates = [];
    getCount = 0;
    failNextCreate = false;
    requests = [];
    // Match root API requests, not Vite module URLs under /src/shared/api/.
    await page.route((url) => url.pathname.startsWith('/api/'), async (route) => {
      const request = route.request();
      const path = new URL(request.url()).pathname;
      requests.push(path);
      if (path === '/api/public/v1/payment-config') {
        await route.fulfill({ json: { provider: 'PAYOS', bank: 'KienlongBank', staticUrl: new URL('/pay', baseURL ?? 'http://127.0.0.1:5173').href, minAmountVnd: 1, maxAmountVnd: 500_000_000, ready: true, status: 'READY' } });
      } else if (path === '/api/public/v1/payments' && request.method() === 'POST') {
        creates.push({ key: request.headers()['idempotency-key'], amountVnd: request.postDataJSON().amountVnd });
        if (failNextCreate) { failNextCreate = false; await route.abort('failed'); }
        else await route.fulfill({ status: 201, json: order });
      } else if (path === `/api/public/v1/payments/${id}`) {
        getCount++;
        await route.fulfill({ json: order });
      } else await route.abort();
    });
  });

  test('creates only on submit, uses exact VND, and recovers an uncertain POST with the same key after reload', async ({ page }) => {
    await page.goto('/pay');
    const input = page.getByLabel('Số tiền thanh toán (VND)');
    const create = page.getByRole('button', { name: 'Tạo đơn thanh toán', exact: true });
    await input.fill('-50');
    await expect(create).toBeDisabled();
    await input.fill('50.5');
    await expect(create).toBeDisabled();
    await input.fill('50000');
    await expect(create).toBeEnabled();
    expect(creates).toHaveLength(0);
    failNextCreate = true;
    await create.click();
    await expect(page.getByRole('alert')).toBeVisible();
    expect(creates).toHaveLength(1);
    expect(creates[0].amountVnd).toBe(50_000);
    const originalKey = creates[0].key;
    expect(originalKey).toMatch(/^[a-f0-9-]{36}$/);
    await page.reload();
    await expect(input).toHaveValue('50000');
    await expect(input).toBeDisabled();
    await page.getByRole('button', { name: 'Thử lại yêu cầu đã lưu' }).click();
    await expect(page).toHaveURL(new RegExp(`/pay/${id}$`));
    await expect(page.getByRole('img', { name: /QR payOS thanh toán/ })).toHaveAttribute('src', /^data:image\/png;base64,/);
    expect(creates).toHaveLength(2);
    expect(creates[1]).toEqual({ key: originalKey, amountVnd: 50_000 });
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Đang chờ thanh toán' })).toBeVisible();
    expect(creates).toHaveLength(2);
    expect(requests.some((path) => path.startsWith('/api/v1/') || path.includes('/tts'))).toBe(false);
  });

  test('ignores return hints, uses authoritative fallback snapshots, and removes paid QR/checkout', async ({ page }) => {
    await page.goto(`/pay/${id}?status=PAID&success=true`);
    await expect(page.getByRole('heading', { name: 'Đang chờ thanh toán' })).toBeVisible();
    await expect(page.getByText('VA000001', { exact: true })).toBeVisible();
    const checkout = page.getByRole('link', { name: 'Thanh toán trên payOS' });
    await expect(checkout).toHaveAttribute('href', order.checkoutUrl!);
    await expect(checkout).toHaveAttribute('rel', 'noopener noreferrer');
    const initialGets = getCount;
    order = { ...order, status: 'PAID', paidAt: new Date().toISOString() };
    // SSE is aborted by the fixture; the five-second GET fallback must recover.
    await expect(page.getByRole('heading', { name: 'Thanh toán thành công' })).toBeVisible({ timeout: 8_000 });
    expect(getCount).toBeGreaterThan(initialGets);
    await expect(page.getByRole('img', { name: /QR payOS/ })).toHaveCount(0);
    await expect(checkout).toHaveCount(0);
    await expect(page.getByText(/Không thanh toán lại đơn này/)).toBeVisible();
    expect(creates).toHaveLength(0);
  });

  test('explicit new-order action after a terminal snapshot uses a new key', async ({ page }) => {
    await page.goto('/pay');
    await page.getByLabel('Số tiền thanh toán (VND)').fill('50000');
    await page.getByRole('button', { name: 'Tạo đơn thanh toán', exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/pay/${id}$`));
    const oldKey = creates[0].key;
    order = { ...order, status: 'EXPIRED' };
    // The fallback GET may already remove the pending-state button.
    await page.evaluate(() => window.dispatchEvent(new Event('pageshow')));
    await expect(page.getByRole('heading', { name: 'Đơn đã hết hạn' })).toBeVisible();
    await expect(page.getByRole('img', { name: /QR payOS/ })).toHaveCount(0);
    await expect(page.getByRole('link', { name: 'Thanh toán trên payOS' })).toHaveCount(0);
    await page.getByRole('button', { name: 'Tạo đơn mới', exact: true }).click();
    await page.getByLabel('Số tiền thanh toán (VND)').fill('50000');
    await page.getByRole('button', { name: 'Tạo đơn thanh toán', exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/pay/${id}$`));
    expect(creates).toHaveLength(2);
    expect(creates[1].key).not.toBe(oldKey);
  });
});
