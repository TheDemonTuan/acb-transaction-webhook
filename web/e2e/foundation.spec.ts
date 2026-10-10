import { expect, test } from '@playwright/test';


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
  await expect(page.getByText(name)).toBeVisible();
  await page.getByRole('button', { name: /Enable|Kích hoạt/ }).last().click();
  await expect(page.getByText(/ACTIVE|Hoạt động/, { exact: true }).last()).toBeVisible();
});


test('navigates payment and diagnostic pages without bank credentials', async ({ page }) => {
  await page.goto('/admin/connection');
  await expect(page.getByRole('heading', { name: 'Kết nối ngân hàng' })).toBeVisible();

  await page.getByRole('button', { name: 'Giao dịch' }).click();
  await expect(page).toHaveURL(/\/transactions$/);

  await page.getByRole('button', { name: 'Phân phối' }).click();
  await expect(page.getByRole('heading', { name: /Phân phối/ })).toBeVisible();

  await page.getByRole('button', { name: 'Chẩn đoán' }).click();
  await expect(page.getByRole('heading', { name: 'Chẩn đoán hệ thống' })).toBeVisible();

  await page.getByRole('button', { name: 'Audit' }).click();
  await expect(page.getByRole('heading', { name: 'Audit Logs' })).toBeVisible();
});
