import { expect, test } from '@playwright/test';

test.describe('V3 Features: Schedule, QR, Server-side Transactions & Detail', () => {
  test('configures schedule polling and verifies transition preview', async ({ page }) => {
    await page.goto('/admin/connection');
    await expect(page.getByRole('heading', { name: 'Lịch trình quét ACB & Giữ phiên (Schedule Polling)' })).toBeVisible();

    // Verify live status card shows current mode
    await expect(page.getByText(/Trạng thái hiện tại:/)).toBeVisible();
    await expect(page.getByText(/Lần chuyển đổi tiếp theo:/)).toBeVisible();

    // Click preset "Chuẩn V3"
    await page.getByRole('button', { name: /Chuẩn V3/ }).click();
    await expect(page.getByText(/Đã áp dụng mẫu cấu hình/)).toBeVisible();

    await expect(page.getByText(/07:00–23:00 Realtime \(3–10s\)/)).toBeVisible();
    await expect(page.getByText(/Ngoài giờ Giữ phiên \(1–2p\)/)).toBeVisible();

    // Verify draft summary appears immediately upon preset selection
    await expect(page.getByText(/Tóm tắt bản nháp cấu hình/)).toBeVisible();
    await expect(page.getByText(/Mẫu chuẩn: hằng ngày 07:00–23:00/)).toBeVisible();

    // Verify day-of-week controls are rendered and interactive
    const monButton = page.getByRole('button', { name: 'Thứ 2', exact: true }).first();
    await expect(monButton).toBeVisible();
    await expect(monButton).toHaveAttribute('aria-pressed', 'true');

    const saveResponse = page.waitForResponse(
      (response) => response.url().endsWith('/api/v1/monitor/settings') && response.request().method() === 'POST',
    );
    await page.getByRole('button', { name: 'Lưu thay đổi lịch trình' }).click();
    expect((await saveResponse).status()).toBe(200);
    await expect(page.getByText('Đã lưu cấu hình lịch trình polling thành công!')).toBeVisible();

    await page.reload();
    await expect(page.locator('input[type="number"][value="3"]').first()).toBeVisible();
    await expect(page.locator('input[type="number"][value="10"]').first()).toBeVisible();

    await page.getByRole('button', { name: 'Thêm khung giờ' }).click();
    await expect(page.getByPlaceholder('Tên khung giờ (ví dụ: Ban ngày)').last()).toBeVisible();
  });

  test('validates schedule days of week and presets interaction', async ({ page }) => {
    await page.goto('/admin/connection');
    await expect(page.getByRole('heading', { name: 'Lịch trình quét ACB & Giữ phiên (Schedule Polling)' })).toBeVisible();

    // Click "Giờ hành chính T2-T6"
    await page.getByRole('button', { name: /Giờ hành chính T2-T6/ }).click();
    await expect(page.getByText(/Giờ hành chính: Thứ 2–Thứ 6, 08:00–18:00/)).toBeVisible();

    // Verify Saturday and Sunday are NOT pressed, while Monday is pressed
    const satBtn = page.getByRole('button', { name: 'Thứ 7', exact: true }).first();
    const sunBtn = page.getByRole('button', { name: 'Chủ nhật', exact: true }).first();
    const monBtn = page.getByRole('button', { name: 'Thứ 2', exact: true }).first();

    await expect(satBtn).toHaveAttribute('aria-pressed', 'false');
    await expect(sunBtn).toHaveAttribute('aria-pressed', 'false');
    await expect(monBtn).toHaveAttribute('aria-pressed', 'true');

    // Deselect all days by clicking T2, T3, T4, T5, T6
    await monBtn.click();
    await page.getByRole('button', { name: 'Thứ 3', exact: true }).first().click();
    await page.getByRole('button', { name: 'Thứ 4', exact: true }).first().click();
    await page.getByRole('button', { name: 'Thứ 5', exact: true }).first().click();
    await page.getByRole('button', { name: 'Thứ 6', exact: true }).first().click();

    // Save button should now be disabled due to empty days validation
    await expect(page.getByText(/Chọn ít nhất 1 ngày/)).toBeVisible();
    await expect(page.getByRole('button', { name: 'Lưu thay đổi lịch trình' })).toBeDisabled();

    // Re-enable with quick action "Thứ 2–Thứ 6"
    await page.getByRole('button', { name: 'Thứ 2–Thứ 6' }).first().click();
    await expect(monBtn).toHaveAttribute('aria-pressed', 'true');
    await expect(page.getByRole('button', { name: 'Lưu thay đổi lịch trình' })).toBeEnabled();
  });

  test('interacts with Payment QR settings in admin', async ({ page }) => {
    await page.goto('/admin/connection');
    await expect(page.getByRole('heading', { name: 'Mã QR tĩnh nhận tiền (Payment QR)' })).toBeVisible();

    // Verify account number & name inputs
    const accInput = page.getByPlaceholder(/Ví dụ: 123456789/).first();
    await expect(accInput).toBeVisible();
    await accInput.fill('987654321');

    const nameInput = page.getByPlaceholder(/Ví dụ: NGUYEN VAN A/).first();
    await expect(nameInput).toBeVisible();
    await nameInput.fill('NGUYEN VAN TEST');

    // Buttons are visible and enabled
    await expect(page.getByRole('button', { name: 'Tải ảnh QR có sẵn' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Tạo VietQR tự động' })).toBeVisible();
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
    const qrBtn = page.getByRole('button', { name: /Mã QR nhận tiền/ });
    if (await qrBtn.isVisible()) {
      await qrBtn.click();
      await expect(page.getByRole('heading', { name: 'Quét mã nhận tiền ACB' })).toBeVisible();

      // Close modal
      await page.getByRole('button', { name: 'Đóng', exact: true }).click();
      await expect(page.getByRole('heading', { name: 'Quét mã nhận tiền ACB' })).not.toBeVisible();
    }
  });

  test('navigates cleanly across admin overview, connection and transactions', async ({ page }) => {
    await page.goto('/admin');
    await expect(page.getByRole('heading', { name: 'Tổng quan' })).toBeVisible();

    // Navigate to Bank Connection
    await page.getByRole('button', { name: 'Kết nối ACB' }).first().click();
    await expect(page.getByRole('heading', { name: 'Kết nối ACB' })).toBeVisible();

    // Navigate to Transactions
    await page.getByRole('button', { name: 'Giao dịch' }).first().click();
    await expect(page.getByRole('heading', { name: 'Giao dịch', exact: true })).toBeVisible();
  });
});
