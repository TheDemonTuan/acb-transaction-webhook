import { expect, test } from '@playwright/test';

test.describe('Baseline Routes Smoke (Desktop & Mobile)', () => {
  test('navigates /admin and /admin/overview', async ({ page }) => {
    await page.goto('/admin');
    await expect(page.getByRole('heading', { name: 'T???ng quan' })).toBeVisible();
    await expect(page.getByText(/MONITORING|??ang ho???t ?????ng|T??i kho???n k???t n???i/).first()).toBeVisible();

    await page.goto('/admin/overview');
    await expect(page.getByRole('heading', { name: 'T???ng quan' })).toBeVisible();
    await expect(page.getByText(/MONITORING|??ang ho???t ?????ng|T??i kho???n k???t n???i/).first()).toBeVisible();
  });

  test('navigates /admin/connection', async ({ page }) => {
    await page.goto('/admin/connection');
    await expect(page.getByRole('heading', { name: 'K???t n???i ACB' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Tr???ng th??i k???t n???i' })).toBeVisible();
    await expect(page.getByRole('heading', { name: '????ng nh???p & X??c th???c ACB' })).toBeVisible();
    await expect(page.getByRole('heading', { name: /L???ch tr??nh qu??t ACB/ })).toBeVisible();
    await expect(page.getByRole('heading', { name: /M?? QR t??nh nh???n ti???n/ })).toBeVisible();
  });

  test('navigates /admin/notifications', async ({ page }) => {
    await page.goto('/admin/notifications');
    await expect(page.getByRole('heading', { name: 'K??nh th??ng b??o', exact: true })).toBeVisible();
    await expect(page.getByRole('button', { name: /Bark/ }).first()).toBeVisible();
    await expect(page.getByRole('button', { name: /Webhook/ }).first()).toBeVisible();
  });

  test('navigates /admin/activity across all tabs', async ({ page }) => {
    await page.goto('/admin/activity');
    await expect(page.getByRole('heading', { name: 'Ho???t ?????ng' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Chu k??? Polling' })).toBeVisible();

    // Deliveries tab
    await page.getByRole('button', { name: 'Ph??n ph???i' }).first().click();
    await expect(page.getByRole('heading', { name: 'Ph??n ph???i th??ng b??o' })).toBeVisible();

    // Audit tab
    await page.getByRole('button', { name: 'Audit' }).first().click();
    await expect(page.getByRole('heading', { name: 'Audit Logs' })).toBeVisible();
  });

  test('navigates /admin/system', async ({ page }) => {
    await page.goto('/admin/system');
    await expect(page.getByRole('heading', { name: 'Ch???n ??o??n h??? th???ng' })).toBeVisible();
    await expect(page.getByText('D???ch v??? l??i').first()).toBeVisible();
    await expect(page.getByText('C?? s??? d??? li???u').first()).toBeVisible();
  });

  test('navigates /transactions and /transactions/:id', async ({ page }) => {
    await page.goto('/transactions');
    await expect(page.getByRole('heading', { name: 'ACB Transaction Webhook' })).toBeVisible();
    await expect(page.getByPlaceholder(/T??m theo n???i dung/)).toBeVisible();

    // Navigate to transaction detail
    await page.goto('/transactions/tx_101');
    await expect(page.getByText('Ti???n v??o t??i kho???n')).toBeVisible();
    await expect(page.getByText('ACB:101')).toBeVisible();
  });
});
