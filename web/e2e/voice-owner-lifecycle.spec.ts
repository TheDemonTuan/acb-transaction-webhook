import { expect, test } from '@playwright/test';
import { emitCredit, expectCompleted, installVoiceFixture, telemetry, unlockVoice } from './voice-audio-fixture';

test.beforeEach(async ({ page }) => {
  await installVoiceFixture(page, { enabled: true });
});

test('restored enabled owner exposes locked state and unlocks without first disabling preference', async ({ page }) => {
  await page.goto('/transactions');
  await expect(page.getByRole('button', { name: 'Mở khóa loa', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Mở khóa loa', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Giọng đọc: Bật', exact: true })).toBeVisible();
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('acb.voice.settings.v1')!).enabled)).toBe(true);
});

test('disable while replay detail is pending prevents late enqueue while later explicit test still works', async ({ page }) => {
  const pageErrors: string[] = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  const detail = { id: 'pending-replay', bank: 'KienlongBank', semanticKey: 'PAYOS:pending-replay', credit: 50000,
    debit: 0, transactionDate: new Date().toISOString(), effectiveDate: new Date().toISOString(),
    firstSeenAt: new Date().toISOString(), description: 'REPLAY DETAIL FIXTURE', source: 'REALTIME' };
  const pending = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  let requests = 0;
  await page.route(/\/api\/(?:public\/)?v1\/transactions\/pending-replay$/, async route => {
    requests++;
    if (requests > 1) { pending.resolve(); await release.promise; }
    await route.fulfill({ json: detail });
  });
  await page.goto('/transactions/pending-replay');
  await page.getByRole('button', { name: 'Mở khóa loa', exact: true }).click();
  await page.getByRole('button', { name: 'Đọc lại giao dịch', exact: true }).click();
  await pending.promise;
  await page.getByRole('button', { name: 'Giọng đọc: Bật', exact: true }).click();
  const response = page.waitForResponse(/\/transactions\/pending-replay$/);
  release.resolve(); await response;
  await page.getByRole('button', { name: 'Tùy chỉnh giọng đọc', exact: true }).click();
  await page.getByRole('button', { name: 'Nghe thử', exact: true }).click();
  const [sample] = await expectCompleted(page, 1);
  expect(sample.isTest).toBe(true);
  expect(sample.isReplay).not.toBe(true);
  await expect(page.getByRole('alert').filter({ hasText: 'VOICE_CANCELLED' })).toHaveCount(0);
  expect(pageErrors).toEqual([]);
});

test('a genuine replay detail failure is visible and handled without an unhandled rejection', async ({ page }) => {
  const pageErrors: string[] = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  const detail = { id: 'failed-replay', bank: 'KienlongBank', semanticKey: 'PAYOS:failed-replay', credit: 50000,
    debit: 0, transactionDate: new Date().toISOString(), effectiveDate: new Date().toISOString(),
    firstSeenAt: new Date().toISOString(), description: 'REPLAY FAILURE FIXTURE', source: 'REALTIME' };
  let requests = 0;
  await page.route(/\/api\/(?:public\/)?v1\/transactions\/failed-replay$/, async route => {
    if (++requests > 1) {
      await route.fulfill({ status: 503, json: { code: 'REPLAY_DETAIL_UNAVAILABLE', error: 'Không thể tải giao dịch để đọc lại' } });
      return;
    }
    await route.fulfill({ json: detail });
  });
  await page.goto('/transactions/failed-replay');
  await page.getByRole('button', { name: 'Mở khóa loa', exact: true }).click();
  await page.getByRole('button', { name: 'Đọc lại giao dịch', exact: true }).click();
  await expect(page.getByRole('alert')).toHaveText('Không thể tải giao dịch để đọc lại');
  expect(await telemetry(page)).toEqual([]);
  expect(pageErrors).toEqual([]);
});

test('client navigation retains one unlocked owner and one realtime subscription', async ({ page }) => {
  await page.goto('/transactions');
  await unlockVoice(page);
  await page.getByRole('button', { name: 'Kết nối ngân hàng' }).first().click();
  await expect(page).toHaveURL(/\/admin\/connection$/);
  await expect(page.getByRole('button', { name: 'Giọng đọc: Bật', exact: true })).toBeVisible();
  await emitCredit(page);
  await expectCompleted(page, 1);
  await page.getByRole('button', { name: /Kênh thông báo|Webhooks/ }).first().click();
  await expect(page.getByRole('heading', { name: 'Kênh thông báo', exact: true })).toBeVisible();
  await emitCredit(page, { transactionId: 'owner-second', orderCode: '100000556678', transactionNumber: '556678' }, 'owner:2');
  await expectCompleted(page, 2);
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('acb.voice.settings.v1')!).enabled)).toBe(true);
});

test('a genuinely suspended context requires explicit unlock and then plays again', async ({ page }) => {
  await page.goto('/transactions');
  await unlockVoice(page);
  await page.getByRole('button', { name: 'Tùy chỉnh giọng đọc', exact: true }).click();
  await page.getByRole('button', { name: 'Nghe thử', exact: true }).click();
  await expectCompleted(page, 1);
  await page.getByRole('button', { name: 'Đóng', exact: true }).click();
  await page.evaluate(async () => {
    const context = window.__voiceContexts[0];
    if (!context) throw new Error('No real destination context observed');
    await context.suspend();
    if (context.state !== 'suspended') throw new Error('Native context did not suspend');
  });
  await emitCredit(page);
  await expect(page.getByRole('button', { name: 'Mở khóa loa', exact: true })).toBeVisible();
  await expect.poll(async () => (await telemetry(page)).filter(sample => sample.outcome === 'failed').length).toBe(1);
  await unlockVoice(page);
  await page.getByRole('button', { name: 'Tùy chỉnh giọng đọc', exact: true }).click();
  await page.getByRole('button', { name: 'Nghe thử', exact: true }).click();
  await expect.poll(async () => (await telemetry(page)).filter(sample => sample.outcome === 'completed').length,
    { timeout: 20000 }).toBe(2);
  const samples = await telemetry(page);
  expect(samples.map(sample => sample.outcome)).toEqual(['completed', 'failed', 'completed']);
  expect(samples.at(-1)?.audioPath).toBe('decoded-pack');
});
