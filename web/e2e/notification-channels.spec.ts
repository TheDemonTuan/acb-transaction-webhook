import { expect, test } from '@playwright/test';

test.describe('Notification Channels & Bark Provider E2E', () => {
  test('creates Bark notification channel, tests notification and manages lifecycle', async ({ page }, testInfo) => {
    const baseName = `iPhone 16 ${testInfo.project.name}`;
    const updatedName = `${baseName} VIP`;

    await page.goto('/admin/notifications');

    // 1. Verify Page Title & Provider Cards
    await expect(page.getByRole('heading', { name: 'Kênh thông báo' }).first()).toBeVisible();
    await expect(page.getByText('Bark • iPhone Push')).toBeVisible();
    await expect(page.getByText('Webhook HMAC')).toBeVisible();

    // 2. Bark tab should be default
    await expect(page.getByRole('button', { name: 'Bark (iPhone)', exact: true })).toBeVisible();
    await expect(page.getByText('Hướng dẫn kết nối Bark trên iPhone:')).toBeVisible();

    // 3. Fill Bark Form
    await page.getByLabel('Tên thiết bị / iPhone').fill(baseName);
    await page.getByLabel('Bark Device Key').fill('secret_bark_device_key_e2e_123');

    // Toggle advanced options
    await page.getByRole('button', { name: /Tùy chỉnh thông báo/ }).click();
    await expect(page.getByLabel('Âm thanh chuông')).toBeVisible();
    await page.getByLabel('Âm thanh chuông').selectOption('bell');

    // Submit creation
    await page.getByRole('button', { name: 'Tạo kênh Bark (iPhone)' }).click();
    await expect(page.getByText('Đã tạo kênh Bark iPhone ở trạng thái DISABLED.')).toBeVisible();

    // 4. Verify Channel in list
    const channelRow = page.getByTestId('channel-row').filter({ hasText: baseName });
    await expect(channelRow).toBeVisible();
    await expect(channelRow.getByText('Bark • iPhone', { exact: true })).toBeVisible();
    await expect(channelRow.getByText('Tạm tắt')).toBeVisible();

    // Device key must NEVER appear in DOM!
    await expect(page.locator('body')).not.toContainText('secret_bark_device_key_e2e_123');
    await expect(channelRow.getByText('Key: ●●●●●●●●')).toBeVisible();

    // 5. Test Notification button
    await channelRow.getByRole('button', { name: 'Gửi thử' }).click();
    await expect(page.getByText(/Bark đã chấp nhận thông báo thử|Bark server URL chưa được cấu hình/)).toBeVisible();

    // 6. Enable Channel
    await channelRow.getByRole('button', { name: 'Kích hoạt' }).click();
    await expect(channelRow.getByText('Hoạt động')).toBeVisible();

    // 7. Edit Channel
    await channelRow.getByRole('button', { name: 'Sửa' }).click();
    await expect(page.getByRole('heading', { name: /Chỉnh sửa kênh:/ })).toBeVisible();
    await page.getByLabel('Tên kênh').fill(updatedName);
    await page.getByRole('button', { name: 'Lưu thay đổi' }).click();
    await expect(page.getByTestId('channel-row').getByText(updatedName)).toBeVisible();

    // 8. Rotate Secret / Key
    const updatedChannelRow = page.getByTestId('channel-row').filter({ hasText: updatedName });
    await updatedChannelRow.getByRole('button', { name: 'Đổi Key' }).click();
    await expect(page.getByRole('heading', { name: /Đổi khóa:/ })).toBeVisible();
    await page.getByPlaceholder('Dán Device Key mới từ ứng dụng Bark').fill('new_rotated_device_key_999');
    await page.getByRole('button', { name: 'Xác nhận đổi khóa' }).click();
    await expect(page.getByText(`Đã cập nhật khóa cho kênh "${updatedName}".`)).toBeVisible();

    // Verify key still not visible
    await expect(page.locator('body')).not.toContainText('new_rotated_device_key_999');

    // 9. Disable Channel
    await updatedChannelRow.getByRole('button', { name: 'Tắt kênh' }).click();
    await expect(updatedChannelRow.getByText('Tạm tắt')).toBeVisible();
  });

  test('creates Webhook channel and displays one-time secret dialog', async ({ page }, testInfo) => {
    const hookName = `Slack Bot ${testInfo.project.name}`;

    await page.goto('/admin/notifications');

    // Switch to Webhook tab
    await page.getByRole('button', { name: 'Webhook', exact: true }).click();
    await expect(page.getByLabel('Tên kênh Webhook')).toBeVisible();
    await expect(page.getByLabel('URL Webhook (HTTPS)')).toBeVisible();

    await page.getByLabel('Tên kênh Webhook').fill(hookName);
    await page.getByLabel('URL Webhook (HTTPS)').fill('https://hooks.slack.com/services/T00/B00/X00');

    await page.getByRole('button', { name: 'Tạo kênh Webhook' }).click();

    // Secret dialog must be displayed once
    await expect(page.getByRole('heading', { name: 'Lưu lại khóa bí mật Webhook (Secret)' })).toBeVisible();
    await page.getByRole('button', { name: 'Tôi đã lưu khóa' }).click();
    await expect(page.getByRole('heading', { name: 'Lưu lại khóa bí mật Webhook (Secret)' })).not.toBeVisible();

    // Channel listed
    await expect(page.getByText(hookName)).toBeVisible();
  });

  test('distinguishes Bark provider status (configured, unconfigured, worker error) and device count with refresh', async ({ page }) => {
    let providerCalls = 0;
    let channelCalls = 0;

    const mockChannels = [
      {
        id: 'ch_bark_1',
        name: 'iPhone 15 Pro',
        provider: 'BARK',
        status: 'ACTIVE',
        revision: 1,
        createdAt: '2026-09-14T00:00:00Z',
        updatedAt: '2026-09-14T00:00:00Z',
      },
      {
        id: 'ch_bark_2',
        name: 'iPhone 14',
        provider: 'BARK',
        status: 'DISABLED',
        revision: 1,
        createdAt: '2026-09-14T00:00:00Z',
        updatedAt: '2026-09-14T00:00:00Z',
      },
    ];

    let currentProvidersResponse: { status: number; body: Record<string, unknown> } = {
      status: 200,
      body: {
        providers: [
          { id: 'WEBHOOK', name: 'Webhook', configured: true },
          { id: 'BARK', name: 'Bark (iOS)', configured: true, publicUrl: 'https://bark.worker.site' },
        ],
      },
    };

    await page.route('**/api/v1/notification-channels', async (route) => {
      channelCalls++;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items: mockChannels }),
      });
    });

    await page.route('**/api/v1/notification-providers', async (route) => {
      providerCalls++;
      await route.fulfill({
        status: currentProvidersResponse.status,
        contentType: 'application/json',
        body: JSON.stringify(currentProvidersResponse.body),
      });
    });

    await page.goto('/admin/notifications');

    // 1. Configured state: "Đã cấu hình", public URL, device count (2 đã lưu, 1 đang bật)
    await expect(page.getByText('Đã cấu hình')).toBeVisible();
    await expect(page.getByText('Server URL: https://bark.worker.site')).toBeVisible();
    await expect(page.getByText('2 thiết bị đã lưu (1 đang bật)')).toBeVisible();

    // 2. Refresh button refreshes both channels and providers
    const initialProviderCalls = providerCalls;
    const initialChannelCalls = channelCalls;
    await page.getByRole('button', { name: 'Làm mới' }).click();
    await expect.poll(() => providerCalls).toBeGreaterThan(initialProviderCalls);
    await expect.poll(() => channelCalls).toBeGreaterThan(initialChannelCalls);

    // 3. Worker error / version skew (503): displays "Không xác định được trạng thái", devices still usable
    currentProvidersResponse = {
      status: 503,
      body: { error: 'worker_unavailable' },
    };
    await page.getByRole('button', { name: 'Làm mới' }).click();
    await expect(page.getByText('Không xác định được trạng thái')).toBeVisible();
    // Device list and device count remain usable
    await expect(page.getByText('2 thiết bị đã lưu (1 đang bật)')).toBeVisible();
    await expect(page.getByText('iPhone 15 Pro')).toBeVisible();
    await expect(page.getByText('iPhone 14')).toBeVisible();
    // Must NOT conclude missing URL or connected
    await expect(page.locator('body')).not.toContainText('Đã kết nối');
    await expect(page.locator('body')).not.toContainText('Chưa cấu hình URL');

    // 4. Unconfigured state: displays "Chưa cấu hình"
    currentProvidersResponse = {
      status: 200,
      body: {
        providers: [
          { id: 'WEBHOOK', name: 'Webhook', configured: true },
          { id: 'BARK', name: 'Bark (iOS)', configured: false },
        ],
      },
    };
    await page.getByRole('button', { name: 'Làm mới' }).click();
    await expect(page.getByText('Chưa cấu hình')).toBeVisible();
    await expect(page.locator('body')).not.toContainText('Đã kết nối');
  });
});
