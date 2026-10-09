import { expect, test } from '@playwright/test';

test.describe('Payment configuration, QR, server-side transactions and detail', () => {
  test('shows readonly payOS configuration and reconciliation controls', async ({ page }) => {
    await page.goto('/admin/connection');
    await expect(page.getByRole('heading', { name: 'Kết nối payOS / KienlongBank' })).toBeVisible();
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
    await expect(page.locator('input[type=password]')).toHaveCount(0);
  });

  test('downloads the fixed payment URL QR and preserves WiFi configuration', async ({ page }) => {
    await page.goto('/admin/connection');
    await expect(page.getByRole('heading', { name: 'QR URL cố định · payOS' })).toBeVisible();
    await expect(page.getByRole('img', { name: 'QR cố định mở trang nhập số tiền' })).toHaveAttribute('src', /^data:image\/png/);
    await expect(page.getByRole('link', { name: 'Tải ảnh QR' })).toHaveAttribute('download', 'payment-url-qr.png');
    await expect(page.getByRole('button', { name: 'Sao chép liên kết' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Lưu cấu hình WiFi' })).toBeVisible();
    await expect(page.locator('input[type=file]')).toHaveCount(0);
  });

  test('exercises Transactions Viewer filters, KPI summary, and QR modal', async ({ page }) => {
    await page.goto('/transactions');
    await expect(page.getByRole('heading', { name: 'Giao dịch', exact: true })).toBeVisible();

    // 1. Verify 3 KPI cards are rendered
    await expect(page.getByText(/Tổng số giao dịch|Giao dịch hôm nay/).first()).toBeVisible();
    await expect(page.getByText(/Tổng tiền vào|Tiền vào hôm nay/).first()).toBeVisible();
    await expect(page.getByText(/Tổng tiền ra|Tiền ra hôm nay/).first()).toBeVisible();

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
    await searchInput.fill('test order');
    await page.waitForTimeout(350); // wait for debounce
    await searchInput.clear();

    // 6. QR Code modal in header
    const qrBtn = page.getByRole('button', { name: 'Mã QR nhận tiền', exact: true });
    await qrBtn.click();
    await expect(page.getByRole('dialog', { name: 'Nhận tiền payOS' })).toBeVisible();
    await page.getByRole('button', { name: 'QR cố định', exact: true }).click();
    await expect(page.getByRole('img', { name: 'QR cố định mở trang nhập số tiền' })).toBeVisible();
    await page.getByRole('button', { name: 'Đóng mã QR' }).click();
    await expect(page.getByRole('dialog', { name: 'Nhận tiền payOS' })).not.toBeVisible();
  });

  test('navigates cleanly across admin overview, connection and transactions', async ({ page }) => {
    await page.goto('/admin');
    await expect(page.getByRole('heading', { name: 'Tổng quan' })).toBeVisible();

    // Navigate to Bank Connection
    await page.getByRole('button', { name: 'Kết nối payOS' }).first().click();
    await expect(page.getByRole('heading', { name: 'Kết nối payOS / KienlongBank' })).toBeVisible();

    // Navigate to Transactions
    await page.getByRole('button', { name: 'Giao dịch' }).first().click();
    await expect(page.getByRole('heading', { name: 'Giao dịch', exact: true })).toBeVisible();
  });
});
