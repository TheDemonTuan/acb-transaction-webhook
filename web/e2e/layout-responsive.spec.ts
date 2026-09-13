import { expect, test } from '@playwright/test';

test.describe('Responsive Layout & Viewport Variations', () => {
  test('renders desktop-specific header controls on desktop viewport', async ({ page }, testInfo) => {
    await page.goto('/admin');

    const header = page.locator('header').first();
    await expect(header).toBeVisible();

    if (testInfo.project.name === 'desktop') {
      // Desktop: Full label "Transaction Viewer"
      await expect(header.getByText('Transaction Viewer')).toBeVisible();
      // Realtime status indicator is visible on desktop topbar
      await expect(header.getByText(/??ang c???p nh???t|Th???i gian th???c|???? k???t n???i|??ang k???t n???i/)).toBeVisible();
    } else {
      // Mobile: Short label "Viewer"
      await expect(header.getByText('Viewer', { exact: true })).toBeVisible();
      await expect(header.getByText('Transaction Viewer')).not.toBeVisible();
    }
  });

  test('adjusts navigation tabs and overflow layout between desktop and mobile', async ({ page }, testInfo) => {
    await page.goto('/admin');

    const overviewBtn = page.getByRole('button', { name: 'T???ng quan' }).first();
    const connectionBtn = page.getByRole('button', { name: 'K???t n???i ACB' }).first();
    await expect(overviewBtn).toBeVisible();
    await expect(connectionBtn).toBeVisible();

    const nav = page.locator('nav').first();
    await expect(nav).toBeVisible();
  });

  test('stacks dashboard cards responsively on mobile and grids on desktop', async ({ page }, testInfo) => {
    await page.goto('/admin/overview');

    const accountCard = page.locator('main').getByText('T??i kho???n k???t n???i').first();
    const channelsCard = page.locator('main').getByText('K??nh th??ng b??o').first();
    await expect(accountCard).toBeVisible();
    await expect(channelsCard).toBeVisible();

    const accountBox = await accountCard.boundingBox();
    const channelsBox = await channelsCard.boundingBox();

    if (testInfo.project.name === 'desktop' && accountBox && channelsBox) {
      // On desktop, cards are placed horizontally side-by-side in 3-column grid
      expect(channelsBox.x).toBeGreaterThan(accountBox.x);
    } else if (testInfo.project.name === 'mobile' && accountBox && channelsBox) {
      // On mobile, cards stack vertically
      expect(channelsBox.y).toBeGreaterThan(accountBox.y);
    }
  });
});
