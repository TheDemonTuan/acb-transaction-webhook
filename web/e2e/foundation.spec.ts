import { expect, test } from '@playwright/test';

test('configures a connection and reflects the state across routes', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveTitle('ACB Transaction Webhook — Monitor & Gateway');
  await page.getByRole('button', { name: 'Kết nối ACB' }).click();
  await expect(page.getByRole('heading', { name: 'Kết nối ACB' })).toBeVisible();

  const accountInput = page.getByLabel('Số tài khoản đã che');
  if (await accountInput.isVisible()) {
    await accountInput.fill('***1234');
    await page.getByRole('button', { name: 'Lưu kết nối' }).click();
    await expect(page.getByText('Đã lưu kết nối.')).toBeVisible();
  }
  await expect(page.getByText(/AUTH_REQUIRED|MONITORING|Cần xác thực|Đang hoạt động/).first()).toBeVisible();
  await page.getByRole('button', { name: 'Tổng quan' }).click();
  await expect(page.getByRole('heading', { name: 'Tổng quan' })).toBeVisible();
  await expect(page.getByText(/AUTH_REQUIRED|MONITORING|Cần xác thực|Đang hoạt động/).first()).toBeVisible();
});

test('creates and enables a guarded HTTPS webhook endpoint', async ({ page }, testInfo) => {
  await page.goto('/');
  await page.getByRole('button', { name: /Webhooks|Kênh thông báo/ }).click();
  await expect(page.getByRole('heading', { name: /Webhook endpoints|Kênh thông báo/ })).toBeVisible();

  const webhookTab = page.getByRole('button', { name: 'Webhook', exact: true });
  if (await webhookTab.isVisible()) {
    await webhookTab.click();
  }

  const name = `Receiver ${testInfo.project.name}`;
  await page.getByLabel(/Tên endpoint|Tên kênh Webhook/).fill(name);
  await page.getByLabel(/HTTPS URL|URL Webhook/).fill(`https://events-${testInfo.project.name}.example.com/bank`);
  await page.getByRole('button', { name: /Tạo endpoint|Tạo kênh Webhook/ }).click();
  await expect(page.getByText(/Đã tạo (endpoint|kênh Webhook) ở trạng thái DISABLED\./)).toBeVisible();
  await expect(page.getByText(name)).toBeVisible();
  await page.getByRole('button', { name: /Enable|Kích hoạt/ }).last().click();
  await expect(page.getByText(/ACTIVE|Hoạt động/, { exact: true }).last()).toBeVisible();
});

test('serves the dashboard on a future SPA route', async ({ page }) => {
  await page.goto('/transactions');
  await expect(page.getByRole('heading', { name: 'ACB Transaction Webhook' })).toBeVisible();
  await page.getByRole('button', { name: 'Giao dịch' }).click();
  await expect(page.getByRole('heading', { name: 'Giao dịch' })).toBeVisible();
});

test('activates ACB session to MONITORING and navigates all tabs', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Kết nối ACB' }).click();
  await expect(page.getByRole('heading', { name: 'Kết nối ACB' })).toBeVisible();
  const accountInput = page.getByLabel('Số tài khoản đã che');
  if (await accountInput.isVisible()) {
    await accountInput.fill('***1234');
    await page.getByRole('button', { name: 'Lưu kết nối' }).click();
  }

  const quickBtn = page.getByRole('button', { name: 'Kích hoạt nhanh (Simulate/Verify)' });
  if (await quickBtn.count()) {
    await quickBtn.click();
    await expect(page.getByText('Phiên ACB đang hoạt động')).toBeVisible();
  }

  await page.getByRole('button', { name: 'Giao dịch' }).click();
  await expect(page.getByRole('heading', { name: 'Giao dịch' })).toBeVisible();

  await page.getByRole('button', { name: 'Phân phối' }).click();
  await expect(page.getByRole('heading', { name: /Phân phối/ })).toBeVisible();

  await page.getByRole('button', { name: 'Polling' }).click();
  await expect(page.getByRole('heading', { name: 'Chu kỳ Polling' })).toBeVisible();

  await page.getByRole('button', { name: 'Chẩn đoán' }).click();
  await expect(page.getByRole('heading', { name: 'Chẩn đoán hệ thống' })).toBeVisible();

  await page.getByRole('button', { name: 'Audit' }).click();
  await expect(page.getByRole('heading', { name: 'Audit Logs' })).toBeVisible();
});
