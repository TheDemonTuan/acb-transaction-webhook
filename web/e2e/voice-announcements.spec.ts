import { expect, test } from '@playwright/test';
import { emitCredit, expectCompleted, installVoiceFixture, manifest, profile, telemetry, unlockVoice } from './voice-audio-fixture';

const CALM_DURATION_MS = 1655.4648526077098;
// Performance timestamps and the native audio clock are sampled on different scheduling boundaries.
const NATIVE_AUDIO_SCHEDULING_TOLERANCE_MS = 50;

for (const cold of [false, true]) {
  test(`real VieNeu ${cold ? 'cold stream' : 'decoded pack'} unlock and test playback`, async ({ page }, testInfo) => {
    const requests = await installVoiceFixture(page, { enabled: true, cold });
    await page.goto('/transactions');
    await unlockVoice(page);
    await page.getByRole('button', { name: 'Tùy chỉnh giọng đọc', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Cài đặt đọc giao dịch' })).toBeVisible();
    await expect(page.getByText('Giọng hiệu lực:', { exact: false })).toBeVisible();
    await testInfo.attach(`vieneu-settings-${cold ? 'cold-stream' : 'decoded-pack'}.png`, {
      body: await page.screenshot({ fullPage: true }), contentType: 'image/png',
    });
    await page.getByRole('button', { name: 'Nghe thử', exact: true }).click();
    const [sample] = await expectCompleted(page, 1, cold ? 'cold-stream' : 'decoded-pack');
    expect(sample.isTest).toBe(true);
    expect(sample.chimeStartedMonotonicAt).toBeDefined();
    expect(sample.speechStartedMonotonicAt! - sample.chimeStartedMonotonicAt!)
      .toBeGreaterThanOrEqual(CALM_DURATION_MS - NATIVE_AUDIO_SCHEDULING_TOLERANCE_MS);
    expect(requests.filter(url => url.includes('/voice/test/stream'))).toHaveLength(cold ? 1 : 0);
    const signal = await page.evaluate(() => window.__voiceSignal);
    expect(signal.some(frame => frame.at >= sample.chimeStartedMonotonicAt! &&
      frame.at < sample.chimeStartedMonotonicAt! + CALM_DURATION_MS && frame.rms > 0.001)).toBe(true);
    expect(signal.some(frame => frame.at >= sample.speechStartedMonotonicAt! && frame.rms > 0.001)).toBe(true);
    await testInfo.attach('vieneu-playback.json', { contentType: 'application/json', body: JSON.stringify({
      profile, manifest: { synthesisKey: manifest.synthesisKey, packVersion: manifest.packVersion }, requests,
      voiceState: await page.getByText('Giọng hiệu lực:', { exact: false }).locator('..').locator('p').nth(1).innerText(),
      telemetry: await telemetry(page), signal: { frames: signal.length, maxRms: Math.max(...signal.map(frame => frame.rms)) },
    }) });
  });
}

test('real recorded Calm chime finishes before speech; independent credits speak once while snapshots, duplicate, stale and CATCH_UP remain silent', async ({ page }, testInfo) => {
  await installVoiceFixture(page, { enabled: true });
  let historyRequests = 0;
  const transactionDate = new Date().toISOString();
  const rows = [
    { id: 'tx_e2e_realtime_500k', bank: 'KienlongBank', provider: 'PAYOS', orderCode: '100000556677',
      semanticKey: 'PAYOS:first', credit: 500000, description: 'FIRST REALTIME CREDIT', source: 'REALTIME' },
    { id: 'voice-second', bank: 'KienlongBank', provider: 'PAYOS', orderCode: '100000556678',
      semanticKey: 'PAYOS:second', credit: 500000, description: 'SECOND INDEPENDENT CREDIT', source: 'REALTIME' },
    { id: 'history-acb', bank: 'ACB', semanticKey: 'ACB:history', credit: 250000,
      description: 'ACB HISTORY MUST STAY SILENT', source: 'CATCH_UP' },
    { id: 'history-payos', bank: 'KienlongBank', provider: 'PAYOS', orderCode: '100000778899',
      semanticKey: 'PAYOS:history', credit: 250000, description: 'PAYOS HISTORY MUST STAY SILENT', source: 'CATCH_UP' },
  ].map(row => ({ ...row, debit: 0, transactionDate, effectiveDate: transactionDate, firstSeenAt: transactionDate }));
  await page.route(/\/api\/(?:public\/)?v1\/transactions(?:\?.*)?$/, route => {
    historyRequests++;
    return route.fulfill({ json: { items: rows, summary: { count: 4, incoming: 1500000, outgoing: 0 } } });
  });
  await page.goto('/transactions');
  await unlockVoice(page);
  await page.getByRole('tab', { name: /Lịch sử giao dịch/ }).click();
  const history = page.getByRole('tabpanel', { name: 'Lịch sử giao dịch', exact: true });
  await expect(history.getByText('FIRST REALTIME CREDIT', { exact: true })).toBeVisible();
  expect(await telemetry(page)).toEqual([]);

  await emitCredit(page);
  const [first] = await expectCompleted(page, 1);
  expect(first.chimeStartedMonotonicAt).toBeDefined();
  expect(first.speechStartedMonotonicAt! - first.chimeStartedMonotonicAt!)
    .toBeGreaterThanOrEqual(CALM_DURATION_MS - NATIVE_AUDIO_SCHEDULING_TOLERANCE_MS);
  const signal = await page.evaluate(() => window.__voiceSignal);
  expect(signal.some(frame => frame.at >= first.chimeStartedMonotonicAt! &&
    frame.at < first.chimeStartedMonotonicAt! + CALM_DURATION_MS && frame.rms > 0.001)).toBe(true);
  expect(signal.some(frame => frame.at >= first.speechStartedMonotonicAt! && frame.rms > 0.001)).toBe(true);

  // Every transport event refetches authoritative history. The refetch itself is never speech.
  const replayRequests = historyRequests;
  await emitCredit(page, {}, 'voice:2');
  await expect.poll(() => historyRequests).toBeGreaterThan(replayRequests);
  const staleRequests = historyRequests;
  const stale = new Date(Date.now() - 150000).toISOString();
  await emitCredit(page, { transactionId: 'voice-stale', orderCode: '100000999999', transactionNumber: '999999',
    credit: '1000000', detectedAt: stale, transactionDate: stale }, 'voice:3');
  await expect.poll(() => historyRequests).toBeGreaterThan(staleRequests);
  for (const [index, bank] of ['ACB', 'KienlongBank'].entries()) {
    const before = historyRequests;
    await emitCredit(page, { bank, transactionId: `history-${index}`, source: 'CATCH_UP', credit: '250000',
      orderCode: `10000077889${index}`, transactionNumber: `77889${index}` }, `voice:${4 + index}`);
    await expect.poll(() => historyRequests).toBeGreaterThan(before);
  }
  await expect(history.getByText('ACB HISTORY MUST STAY SILENT', { exact: true })).toBeVisible();
  await expect(history.getByText('PAYOS HISTORY MUST STAY SILENT', { exact: true })).toBeVisible();
  await emitCredit(page, { transactionId: 'voice-second', orderCode: '100000556678', transactionNumber: '556678',
    description: 'SECOND INDEPENDENT CREDIT' }, 'voice:6');
  const samples = await expectCompleted(page, 2);
  expect(samples.every(sample => !sample.isTest && !sample.isReplay)).toBe(true);
  await expect(history.getByText('SECOND INDEPENDENT CREDIT', { exact: true })).toBeVisible();
  await testInfo.attach('vieneu-realtime.json', { contentType: 'application/json', body: JSON.stringify({
    profile, manifest: { synthesisKey: manifest.synthesisKey, packVersion: manifest.packVersion }, telemetry: samples,
    chimeToSpeechMs: first.speechStartedMonotonicAt! - first.chimeStartedMonotonicAt!,
    signal: { frames: signal.length, maxRms: Math.max(...signal.map(frame => frame.rms)) },
  }) });
});

test('unsaved custom draft never changes the confirmed VieNeu request', async ({ page }) => {
  await installVoiceFixture(page, { enabled: true });
  await page.route(/\/api\/v1\/status$/, async route => {
    const response = await route.fetch();
    return route.fulfill({ response, json: { ...await response.json(), userRole: 'OWNER' } });
  });
  let saves = 0;
  page.on('request', request => {
    if (new URL(request.url()).pathname === '/api/v1/voice/settings' && request.method() === 'PUT') saves++;
  });
  await page.goto('/transactions');
  await unlockVoice(page);
  await page.getByRole('button', { name: 'Tùy chỉnh giọng đọc', exact: true }).click();
  const template = page.getByPlaceholder('Ví dụ: Đã nhận {amount} từ khách quý.');
  await template.fill('Bản nháp chưa lưu: {amount}.');
  await page.getByRole('button', { name: 'Nghe thử', exact: true }).click();
  await expectCompleted(page, 1);
  await expect(template).toHaveValue('Bản nháp chưa lưu: {amount}.');
  expect(saves).toBe(0);
});

test('owner settings expose only fixed natural VieNeu without legacy provider controls', async ({ page }) => {
  await installVoiceFixture(page);
  await page.route(/\/api\/v1\/status$/, async route => {
    const response = await route.fetch();
    return route.fulfill({ response, json: { ...await response.json(), userRole: 'OWNER' } });
  });
  await page.goto('/transactions');
  await page.getByRole('button', { name: 'Tùy chỉnh giọng đọc', exact: true }).click();
  await expect(page.getByText('VieNeu · Mai Anh · nữ miền Bắc', { exact: true })).toBeVisible();
  await expect(page.getByText('Tốc độ tự nhiên 1x · Cao độ 0 Hz · Không chuyển sang giọng khác', { exact: true })).toBeVisible();
  await expect(page.getByLabel('Chế độ phát', { exact: true })).toHaveCount(0);
  await expect(page.getByLabel('Giọng máy chủ', { exact: true })).toHaveCount(0);
  await expect(page.getByRole('slider', { name: 'Tốc độ đọc', exact: true })).toHaveCount(0);
  await expect(page.getByRole('slider', { name: 'Cao độ (Pitch)', exact: true })).toHaveCount(0);
  const voiceSwitch = page.getByRole('switch', { name: 'Bật đọc giao dịch', exact: true });
  await voiceSwitch.click();
  await expect(voiceSwitch).toHaveAttribute('aria-checked', 'true');
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('acb.voice.settings.v1')!).enabled)).toBe(true);
});
