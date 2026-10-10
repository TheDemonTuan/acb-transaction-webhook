import { expect, test } from '@playwright/test';

test.describe('Payment configuration, QR, server-side transactions and detail', () => {
  test('shows readonly payOS configuration and reconciliation controls', async ({ page }) => {
    await page.goto('/admin/connection');
    await expect(page.getByRole('heading', { name: 'Kết nối ngân hàng' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Đơn cần đối soát' })).toBeVisible();
    await expect(page.getByLabel('Trạng thái đơn')).toHaveValue('CREATING');
    const processingResponse = page.waitForResponse(response => {
      const url = new URL(response.url());
      return url.pathname === '/api/v1/payments' && url.searchParams.get('status') === 'PROCESSING' && response.request().method() === 'GET';
    });
    await page.getByLabel('Trạng thái đơn').selectOption('PROCESSING');
    const response = await processingResponse;
    expect(response.status()).toBe(200);
    const body = await response.json() as { items: Array<{ status: string }> };
    expect(Array.isArray(body.items)).toBe(true);
    expect(body.items.every(order => order.status === 'PROCESSING')).toBe(true);
    await expect(page.getByLabel('Trạng thái đơn')).toHaveValue('PROCESSING');
  });

  test('downloads the payOS link QR and preserves WiFi configuration in cashier utilities', async ({ page }) => {
    await page.goto('/admin/connection');
    const download = page.getByRole('link', { name: 'Tải ảnh QR', exact: true });
    await expect(download).toHaveAttribute('href', /^data:image\/png/);
    await expect(download).toHaveAttribute('download', 'payment-url-qr.png');
    await page.getByPlaceholder('Ví dụ: ACB_Coffee_Free').fill('Regression WiFi');
    await page.getByPlaceholder('Để trống nếu mạng không cần mật khẩu').fill('wifi-fixture-password');
    await page.getByRole('button', { name: 'Lưu cấu hình WiFi', exact: true }).click();
    await page.goto('/transactions');
    const cashier = page.getByRole('region', { name: 'Thu ngân', exact: true });
    await cashier.getByText('Tiện ích', { exact: true }).click();
    await expect(cashier.getByRole('img', { name: 'Mã QR kết nối WiFi', exact: true })).toHaveAttribute('src', /^data:image\/png/);
    await cashier.getByRole('button', { name: 'Sửa thông tin WiFi', exact: true }).click();
    await expect(cashier.getByLabel('Tên WiFi (SSID)', { exact: true })).toHaveValue('Regression WiFi');
    await expect(cashier.getByLabel('Mật khẩu WiFi', { exact: true })).toHaveValue('wifi-fixture-password');
  });

  test('retains server-side history filters while cashier stays inline', async ({ page }) => {
    await page.goto('/transactions');
    const cashier = page.getByRole('region', { name: 'Thu ngân', exact: true });
    await expect(cashier).toBeVisible();
    const amount = cashier.getByLabel('Số tiền · nghìn đồng');
    await amount.fill('120');
    // 2. Date filter tabs
    await page.getByRole('button', { name: 'Hôm nay' }).click();
    await page.getByRole('button', { name: '7 ngày' }).click();
    await page.getByRole('button', { name: 'Tất cả' }).first().click();

    // 3. Custom date range
    await page.getByRole('button', { name: 'Tùy chọn' }).click();
    await expect(page.getByText('Từ ngày:')).toBeVisible();
    await expect(page.getByText('Đến ngày:')).toBeVisible();

    // 4. Direction filters
    await page.getByRole('button', { name: 'Tiền vào' }).click();
    await page.getByRole('button', { name: 'Tiền ra' }).click();
    await page.getByRole('button', { name: 'Tất cả' }).last().click();

    // 5. Search box
    const searchInput = page.getByPlaceholder('Tìm theo nội dung chuyển khoản, số tiền, mã giao dịch...');
    await expect(searchInput).toBeVisible();
    const searchResponse = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname === '/api/v1/transactions' && url.searchParams.get('q') === 'test order';
    });
    await searchInput.fill('test order');
    expect((await searchResponse).status()).toBe(200);
    await searchInput.clear();

    await expect(amount).toHaveValue('120');
    await expect(cashier).toBeVisible();
    await page.getByRole('link', { name: 'Thu tiền', exact: true }).click();
    await expect(page).toHaveURL(/#counter-checkout$/);
    await expect(amount).toHaveValue('120');
    await expect(page.getByRole('dialog')).toHaveCount(0);
  });

  test('navigates cleanly across admin overview, connection and transactions', async ({ page }) => {
    await page.goto('/admin');
    await expect(page.getByRole('heading', { name: 'Tổng quan' })).toBeVisible();

    // Navigate to Bank Connection
    await page.getByRole('button', { name: 'Kết nối ngân hàng' }).first().click();
    await expect(page).toHaveURL(/\/admin\/connection$/);

    // Navigate to Transactions
    await page.getByRole('button', { name: 'Giao dịch' }).first().click();
    await expect(page).toHaveURL(/\/transactions$/);
    await expect(page.getByRole('region', { name: 'Thu ngân', exact: true })).toBeVisible();
  });
});
