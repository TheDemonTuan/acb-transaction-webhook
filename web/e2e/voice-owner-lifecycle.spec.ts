import { expect, test } from '@playwright/test';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';

test.beforeEach(async ({ page }) => {
  await page.route(/\/api\/(?:public\/)?v1\/voice\/profile$/, route => route.fulfill({ json: {
    ...DEFAULT_VOICE_PROFILE, providerMode: 'BROWSER_ONLY', synthesisKey: 'a'.repeat(64),
    funEnabled: false, chimeEnabled: false,
  } }));
  await page.addInitScript(() => {
    localStorage.setItem('acb.voice.settings.v1', JSON.stringify({ schemaVersion: 2, enabled: true,
      volume: 1, announceWhenHidden: true, browserVoiceURI: 'exact-vi' }));
    const spoken: string[] = [];
    Object.defineProperty(window, '__ownerSpeech', { value: spoken });
    class Utterance {
      text: string;
      onstart: (() => void) | null = null;
      onend: (() => void) | null = null;
      constructor(text: string) { this.text = text; }
    }
    Object.defineProperty(window, 'SpeechSynthesisUtterance', { value: Utterance, configurable: true });
    Object.defineProperty(window, 'speechSynthesis', { configurable: true, value: {
      paused: false,
      getVoices: () => [{ lang: 'vi-VN', voiceURI: 'exact-vi', name: 'Exact Vietnamese', default: true }],
      speak: (utterance: Utterance) => {
        if (utterance.text.trim()) spoken.push(utterance.text);
        queueMicrotask(() => { utterance.onstart?.(); utterance.onend?.(); });
      }, cancel: () => {}, resume: () => {}, addEventListener: () => {}, removeEventListener: () => {},
    } });
  });
});

test('restored enabled owner exposes locked state and unlocks without first disabling preference', async ({ page }) => {
  await page.goto('/transactions');
  await expect(page.getByRole('button', { name: 'Mở khóa loa', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Mở khóa loa', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Giọng đọc: Bật', exact: true })).toBeVisible();
  expect(await page.evaluate(() => JSON.parse(localStorage.getItem('acb.voice.settings.v1')!).enabled)).toBe(true);
});

test('disable while replay detail is pending prevents late enqueue while later explicit test still works', async ({ page }) => {
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
  await expect.poll(() => page.evaluate(() => {
    const fixture = window as Window & { __ownerSpeech: string[] };
    return fixture.__ownerSpeech;
  })).toEqual(['Đã nhận năm trăm nghìn đồng.']);
});
