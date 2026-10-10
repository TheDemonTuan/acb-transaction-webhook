import { expect, test, type Page } from '@playwright/test';

type Role = 'OWNER' | 'OPERATOR' | 'VIEWER';

async function installSystemFixture(page: Page, role: Role) {
  let reviewReads = 0;
  let statusReads = 0;
  let failReviews = false;
  const reviews = [{ storeKey: 'test-store', messageId: '9007199254740993', reason: 'REFERENCE_CONFLICT', receivedAt: '2026-10-10T12:00:00Z' }];
  await page.route(/\/api\/v1\/status$/, (route) => {
    statusReads += 1;
    return route.fulfill({ json: {
      service: 'HEALTHY', version: 'fixture', uptimeSeconds: 60, userRole: role,
      payments: { provider: 'PAYOS', bank: 'KienlongBank', configured: true, status: 'READY', webhookConfirmed: true, lastWebhookAt: null, lastReconciledAt: null, pendingOrders: 0, reviewCount: 9 },
      sepay: { mode: 'observe', lastMessageAt: '2026-10-10T11:00:00Z', reviewCount: 1 },
      storage: { status: 'READY' }, webhooks: { pending: 0, deadLetter: 0 },
    } });
  });
  await page.route(/\/api\/v1\/sepay-reviews(?:\?.*)?$/, (route) => {
    reviewReads += 1;
    expect(role).not.toBe('VIEWER');
    expect(route.request().method()).toBe('GET');
    expect(new URL(route.request().url()).searchParams.get('limit')).toBe('20');
    return route.fulfill({ status: failReviews ? 503 : 200, json: failReviews ? { error: 'storage_error' } : { items: reviews } });
  });
  return {
    get reviewReads() { return reviewReads; },
    get statusReads() { return statusReads; },
    failReviews() { failReviews = true; },
  };
}

for (const role of ['OWNER', 'OPERATOR'] as const) {
  test(`${role} sees readonly SePay reviews and can refresh without payment mutations`, async ({ page }) => {
    const fixture = await installSystemFixture(page, role);
    const mutations: string[] = [];
    page.on('request', (request) => {
      if (request.url().includes('/api/') && ['POST', 'PUT', 'PATCH', 'DELETE'].includes(request.method())) mutations.push(request.url());
    });
    await page.goto('/admin/system');
    const section = page.getByRole('region', { name: 'SePay Store · Nhận thông báo' });
    await expect(section).toContainText('observe');
    await expect(section).toContainText('2026-10-10T11:00:00Z');
    await expect(section).toContainText('REFERENCE_CONFLICT');
    await expect(section).toContainText('9007199254740993');
    await expect(section.getByRole('listitem')).toHaveCount(1);
    await expect(section.getByRole('button')).toHaveCount(0);
    const before = { status: fixture.statusReads, reviews: fixture.reviewReads };
    await page.getByRole('button', { name: 'Làm mới', exact: true }).click();
    await expect.poll(() => fixture.statusReads).toBeGreaterThan(before.status);
    await expect.poll(() => fixture.reviewReads).toBeGreaterThan(before.reviews);
    expect(mutations).toEqual([]);
    fixture.failReviews();
    await page.getByRole('button', { name: 'Làm mới', exact: true }).click();
    await expect(section.getByRole('alert')).toBeVisible();
    await expect(section).toContainText('observe');
  });
}

test('VIEWER never requests or renders operator reviews, including refresh', async ({ page }) => {
  const fixture = await installSystemFixture(page, 'VIEWER');
  await page.goto('/admin/system');
  const section = page.getByRole('region', { name: 'SePay Store · Nhận thông báo' });
  await expect(section).toContainText('observe');
  await expect(section.getByRole('list')).toHaveCount(0);
  const before = fixture.statusReads;
  await page.getByRole('button', { name: 'Làm mới', exact: true }).click();
  await expect.poll(() => fixture.statusReads).toBeGreaterThan(before);
  await expect(section.getByRole('list')).toHaveCount(0);
  expect(fixture.reviewReads).toBe(0);
});
