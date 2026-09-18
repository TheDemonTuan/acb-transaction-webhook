import { expect, test } from '@playwright/test';

test.describe('Payment QR Public Route & Real <img> Rendering', () => {
  const sample1x1Png = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=',
    'base64'
  );

  test('renders real <img> with complete=true and positive dimensions on /pay/:identifier', async ({ page }) => {
    // 1. Mock public payment-qr metadata
    await page.route('**/api/public/v1/payment-qr', (route) => {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          configured: true,
          hasImage: true,
          imageURL: '/api/public/v1/payment-qr/image?v=10',
          qr: {
            accountName: 'NGUYEN VIET TUAN',
            accountNumber: '9876543210',
            bankName: 'ACB (Á Châu)',
          },
        }),
      });
    });

    // 2. Mock public image endpoint with real PNG bytes
    await page.route('**/api/public/v1/payment-qr/image*', (route) => {
      return route.fulfill({
        status: 200,
        contentType: 'image/png',
        body: sample1x1Png,
      });
    });

    // 3. Mock activation
    await page.route('**/api/public/v1/payment-qr/activate', (route) => {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          trackingActive: true,
          phase: 'GRACE',
        }),
      });
    });

    // 4. Navigate to /pay/:identifier
    await page.goto('/pay/payer-session-001');

    // Verify header and bank details
    await expect(page.getByRole('heading', { name: 'Thanh toán VietQR' })).toBeVisible();
    await expect(page.getByText('9876543210')).toBeVisible();
    await expect(page.getByText('NGUYEN VIET TUAN')).toBeVisible();

    // 5. Assert actual <img> renders and is loaded
    const qrImg = page.locator('img[alt="VietQR Code"]');
    await expect(qrImg).toBeVisible();

    const isImageReal = await qrImg.evaluate((el: HTMLImageElement) => {
      return el.complete && el.naturalWidth > 0 && el.naturalHeight > 0;
    });
    expect(isImageReal).toBe(true);
  });

  test('shows graceful fallback when image fails to load without hiding bank details', async ({ page }) => {
    await page.route('**/api/public/v1/payment-qr', (route) => {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          configured: true,
          hasImage: true,
          imageURL: '/api/public/v1/payment-qr/image?v=broken',
          qr: {
            accountName: 'NGUYEN VIET TUAN',
            accountNumber: '9876543210',
            bankName: 'ACB (Á Châu)',
          },
        }),
      });
    });

    await page.route('**/api/public/v1/payment-qr/image*', (route) => {
      return route.fulfill({
        status: 500,
        body: 'Internal Server Error',
      });
    });

    await page.route('**/api/public/v1/payment-qr/activate', (route) => {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ trackingActive: true, phase: 'GRACE' }),
      });
    });

    await page.goto('/pay/payer-session-error');

    await expect(page.getByText('Không tải được hình ảnh mã QR')).toBeVisible();
    await expect(page.getByText('Vui lòng dùng thông tin tài khoản bên dưới')).toBeVisible();

    // Critical: bank details and deep link must remain accessible
    await expect(page.getByText('9876543210')).toBeVisible();
    await expect(page.getByText('NGUYEN VIET TUAN')).toBeVisible();
    await expect(page.getByRole('link', { name: 'Mở ứng dụng ACB ONE' })).toBeVisible();
  });

  test('shows not configured state when backend reports unconfigured', async ({ page }) => {
    await page.route('**/api/public/v1/payment-qr', (route) => {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          configured: false,
        }),
      });
    });

    await page.route('**/api/public/v1/payment-qr/activate', (route) => {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ trackingActive: true, phase: 'GRACE' }),
      });
    });

    await page.goto('/pay/payer-unconfigured');

    await expect(page.getByText('Mã QR chưa được cài đặt')).toBeVisible();
  });
});
