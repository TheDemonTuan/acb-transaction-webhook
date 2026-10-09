import { expect, test } from '@playwright/test';

test('shows readonly payment configuration across routes', async ({ page }) => {
  await page.goto('/admin/connection');
  await expect(page.getByRole('heading', { name: 'Kết nối payOS / KienlongBank' })).toBeVisible();
  await expect(page.getByText('Bộ khóa payOS:', { exact: true })).toBeVisible();
  await expect(page.locator('input[type=password]')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'Đơn cần đối soát' })).toBeVisible();
  await page.getByRole('button', { name: 'Tổng quan', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Tổng quan' })).toBeVisible();
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
  await expect(page.getByRole('heading', { name: 'payOS Transaction Webhook' })).toBeVisible();
  await page.getByRole('button', { name: 'Giao dịch' }).click();
  await expect(page.getByRole('heading', { name: 'Giao dịch' })).toBeVisible();
});

test('navigates payment and diagnostic pages without bank credentials', async ({ page }) => {
  await page.goto('/admin/connection');
  await expect(page.getByRole('heading', { name: 'Kết nối payOS / KienlongBank' })).toBeVisible();

  await page.getByRole('button', { name: 'Giao dịch' }).click();
  await expect(page.getByRole('heading', { name: 'Giao dịch' })).toBeVisible();

  await page.getByRole('button', { name: 'Phân phối' }).click();
  await expect(page.getByRole('heading', { name: /Phân phối/ })).toBeVisible();

  await page.getByRole('button', { name: 'Chẩn đoán' }).click();
  await expect(page.getByRole('heading', { name: 'Chẩn đoán hệ thống' })).toBeVisible();

  await page.getByRole('button', { name: 'Audit' }).click();
  await expect(page.getByRole('heading', { name: 'Audit Logs' })).toBeVisible();
});
