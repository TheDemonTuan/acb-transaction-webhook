import { expect, test, type Page } from '@playwright/test';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';

type VoiceFixtureWindow = Window & {
  __activeEventSource?: EventSource;
  __spokenUtterances: Array<{ text: string; lang: string; volume: number; rate: number }>;
};

const spokenAnnouncements = (page: Page) => page.evaluate(() =>
  (window as VoiceFixtureWindow).__spokenUtterances.filter(utterance => utterance.text.trim().length > 0));

async function expectTestAmount(page: Page) {
  await expect.poll(async () => (await spokenAnnouncements(page)).map(utterance => utterance.text))
    .toEqual([expect.stringContaining('năm trăm nghìn đồng')]);
  const [announcement] = await spokenAnnouncements(page);
  expect(announcement.lang).toBe('vi-VN');
  expect(announcement.rate).toBe(DEFAULT_VOICE_PROFILE.rate);
  expect(announcement.volume).toBe(1);
  return announcement;
}

test.describe('Voice Announcements & Realtime Features', () => {
  test.beforeEach(async ({ page }) => {
    // Deterministic browser-mode lifecycle fixture, not evidence for Edge audio.
    await page.route(/\/api\/(?:public\/)?v1\/voice\/profile$/, (route) => route.fulfill({
      contentType: 'application/json', body: JSON.stringify({ ...DEFAULT_VOICE_PROFILE, providerMode: 'BROWSER_ONLY',
        synthesisKey: 'a'.repeat(64), funEnabled: false, chimeEnabled: false }),
    }));
    await page.addInitScript(() => {
      localStorage.setItem('acb.voice.settings.v1', JSON.stringify({ schemaVersion: 2, enabled: false, volume: 1,
        announceWhenHidden: true, browserVoiceURI: 'urn:moz-tts:speechd:vi-VN' }));
    });
    await page.addInitScript(() => {
      (window as any).__spokenUtterances = [];

      class MockSpeechSynthesisUtterance {
        text: string;
        lang = 'vi-VN';
        volume = 1;
        rate = 1;
        pitch = 1;
        voice: any = null;
        onend: any = null;
        onstart: ((event: Event) => void) | null = null;
        onerror: any = null;
        constructor(text: string) {
          this.text = text;
        }
      }

      const synth = {
        paused: false,
        speaking: false,
        pending: false,
        onvoiceschanged: null,
        getVoices: () => [
          {
            default: true,
            lang: 'vi-VN',
            localService: true,
            name: 'Vietnamese Female',
            voiceURI: 'urn:moz-tts:speechd:vi-VN',
          },
        ],
        speak: (utterance: any) => {
          (window as any).__spokenUtterances.push({
            text: utterance.text,
            lang: utterance.lang,
            volume: utterance.volume,
            rate: utterance.rate,
          });
          queueMicrotask(() => {
            utterance.onstart?.(new Event('start'));
            utterance.onend?.(new Event('end'));
          });
        },
        cancel: () => {},
        pause: () => {},
        resume: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => true,
      };

      (window as any).SpeechSynthesisUtterance = MockSpeechSynthesisUtterance;
      Object.defineProperty(window, 'speechSynthesis', {
        value: synth,
        configurable: true,
        writable: true,
      });

      // Track active EventSource instance so we can simulate real incoming SSE events
      const OriginalEventSource = window.EventSource;
      window.EventSource = function (url: string | URL, eventSourceInitDict?: EventSourceInit) {
        const es = new OriginalEventSource(url, eventSourceInitDict);
        (window as any).__activeEventSource = es;
        return es;
      } as any;
      window.EventSource.prototype = OriginalEventSource.prototype;
    });
  });

  test('opens voice settings sheet and plays test voice', async ({ page }) => {
    await page.goto('/transactions');
    await expect(page.getByRole('tabpanel', { name: 'Quầy thu ngân', exact: true })).toBeVisible();

    const voiceSettingsBtn = page.getByRole('button', { name: 'Tùy chỉnh giọng đọc' });
    await expect(voiceSettingsBtn).toBeVisible();
    await voiceSettingsBtn.click();

    await expect(page.getByRole('heading', { name: 'Cài đặt đọc giao dịch' })).toBeVisible();
    await expect(page.getByText('Giọng hiệu lực:', { exact: false })).toBeVisible();

    const testBtn = page.getByRole('button', { name: 'Nghe thử' });
    await expect(testBtn).toBeVisible();
    await testBtn.click();

    await expectTestAmount(page);
  });

  test('announces independent KienlongBank orders once each while replay, stale and CATCH_UP stay silent', async ({
    page,
  }) => {
    // Fixed committed snapshots, independent of the simulated SSE payloads.
    // Refetching history must not erase rows or cause snapshot data to speak.
    let historyRequests = 0;
    const transactionDate = new Date().toISOString();
    await page.route(/\/api\/(?:public\/)?v1\/transactions(?:\?.*)?$/, async (route) => {
      historyRequests += 1;
      await route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({
          items: [
            {
              id: 'tx_e2e_realtime_500k', bank: 'KienlongBank', provider: 'PAYOS',
              orderCode: '100000556677', semanticKey: 'PAYOS:e2e:556677',
              credit: 500000, debit: 0, transactionDate, effectiveDate: transactionDate,
              firstSeenAt: transactionDate, source: 'REALTIME',
              description: 'NGUYEN VAN A CHUYEN TIEN REALTIME',
            },
            {
              id: 'tx_catch_up_0', bank: 'ACB', semanticKey: 'ACB:778899',
              credit: 250000, debit: 0, transactionDate, effectiveDate: transactionDate,
              firstSeenAt: transactionDate, source: 'CATCH_UP', description: 'LỊCH SỬ ACB KHÔNG ĐỌC',
            },
            {
              id: 'tx_catch_up_1', bank: 'KienlongBank', provider: 'PAYOS',
              orderCode: '100000778899', semanticKey: 'PAYOS:e2e:778899',
              credit: 250000, debit: 0, transactionDate, effectiveDate: transactionDate,
              firstSeenAt: transactionDate, source: 'CATCH_UP', description: 'LỊCH SỬ KienlongBank KHÔNG ĐỌC',
            },
            {
              id: 'tx_e2e_second_500k', bank: 'KienlongBank', provider: 'PAYOS',
              orderCode: '100000556678', semanticKey: 'PAYOS:e2e:556678',
              credit: 500000, debit: 0, transactionDate, effectiveDate: transactionDate,
              firstSeenAt: transactionDate, source: 'REALTIME', description: 'KHÁCH THỨ HAI CÙNG SỐ TIỀN',
            },
          ],
          summary: { count: 4, incoming: 1500000, outgoing: 0 },
        }),
      });
    });
    await page.goto('/transactions');

    // 1. Enable voice announcement
    await page.getByRole('button', { name: 'Tùy chỉnh giọng đọc' }).click();
    await expect(page.getByRole('heading', { name: 'Cài đặt đọc giao dịch' })).toBeVisible();
    const voiceSwitch = page.getByRole('switch', { name: 'Bật đọc giao dịch' });
    await voiceSwitch.click();
    await expect(voiceSwitch).toHaveAttribute('aria-checked', 'true');
    await page.getByRole('button', { name: 'Đóng' }).click();
    await expect(page.getByRole('heading', { name: 'Cài đặt đọc giao dịch' })).toHaveCount(0);

    const historyTab = page.getByRole('tab', { name: /Lịch sử giao dịch/ });
    await historyTab.click();
    const history = page.getByRole('tabpanel', { name: 'Lịch sử giao dịch', exact: true });
    await expect(history).toBeVisible();
    await expect(history.getByText('NGUYEN VAN A CHUYEN TIEN REALTIME', { exact: true })).toBeVisible();
    expect(await spokenAnnouncements(page)).toEqual([]);

    // The unlock utterance is not a transaction announcement.
    const requestsBeforeCredit = historyRequests;

    // 2. Dispatch genuine bank.transaction.credit SSE event
    await page.evaluate(() => {
      const es = (window as any).__activeEventSource;
      if (!es) throw new Error('EventSource not initialized');

      const event = new MessageEvent('bank.transaction.credit', {
        data: JSON.stringify({
          bank: 'KienlongBank',
          provider: 'PAYOS',
          orderCode: '100000556677',
          transactionId: 'tx_e2e_realtime_500k',
          transactionNumber: '556677',
          credit: '500000',
          debit: '0',
          currency: 'VND',
          transactionDate: new Date().toISOString(),
          source: 'REALTIME',
          description: 'NGUYEN VAN A CHUYEN TIEN REALTIME',
          detectedAt: new Date().toISOString(),
        }),
        lastEventId: 'ep1:888001',
      });
      es.dispatchEvent(event);
    });

    // Wait for the actual amount, not the blank utterance used to unlock audio.
    await expectTestAmount(page);

    // The authoritative history refetch retains committed rows without speaking them.
    await expect.poll(() => historyRequests).toBeGreaterThan(requestsBeforeCredit);
    await expect(history.getByText('NGUYEN VAN A CHUYEN TIEN REALTIME', { exact: true })).toBeVisible();
    await expect(history.getByText('KienlongBank', { exact: true })).toHaveCount(3);
    await expect(history.getByText('ACB', { exact: true })).toBeVisible();

    // 5. Replay the same committed transaction under a different SSE event ID.
    const requestsBeforeReplay = historyRequests;
    await page.evaluate(() => {
      const es = (window as any).__activeEventSource;
      const event = new MessageEvent('bank.transaction.credit', {
        data: JSON.stringify({
          bank: 'KienlongBank',
          provider: 'PAYOS',
          orderCode: '100000556677',
          transactionId: 'tx_e2e_realtime_500k',
          transactionNumber: '556677',
          credit: '500000',
          debit: '0',
          currency: 'VND',
          transactionDate: new Date().toISOString(),
          source: 'REALTIME',
          description: 'NGUYEN VAN A CHUYEN TIEN REALTIME',
          detectedAt: new Date().toISOString(),
        }),
        lastEventId: 'ep1:888002',
      });
      es.dispatchEvent(event);
    });

    await expect.poll(() => historyRequests).toBeGreaterThan(requestsBeforeReplay);
    expect(await spokenAnnouncements(page)).toHaveLength(1);

    // 6. Stale event suppression verification: event older than 150 seconds should be ignored for speech
    const requestsBeforeStale = historyRequests;
    await page.evaluate(() => {
      const es = (window as any).__activeEventSource;
      const staleTime = new Date(Date.now() - 150_000).toISOString();
      const event = new MessageEvent('bank.transaction.credit', {
        data: JSON.stringify({
          bank: 'KienlongBank',
          provider: 'PAYOS',
          orderCode: '100000999999',
          transactionId: 'tx_stale_old',
          transactionNumber: '999999',
          credit: '1000000',
          debit: '0',
          currency: 'VND',
          transactionDate: staleTime,
          source: 'REALTIME',
          description: 'GIAO DICH CU KHONG DUOC DOC',
          detectedAt: staleTime,
        }),
        lastEventId: 'ep1:888003',
      });
      es.dispatchEvent(event);
    });

    await expect.poll(() => historyRequests).toBeGreaterThan(requestsBeforeStale);
    expect(await spokenAnnouncements(page)).toHaveLength(1);

    // Historical ACB and recovered payOS credits update history, never speech.
    const requestsBeforeCatchUp = historyRequests;
    await page.evaluate(() => {
      // Installed by the EventSource fixture in beforeEach.
      const testWindow = window as VoiceFixtureWindow;
      const es = testWindow.__activeEventSource;
      if (!es) throw new Error('EventSource not initialized');
      for (const [index, bank] of ['ACB', 'KienlongBank'].entries()) {
        es.dispatchEvent(new MessageEvent('bank.transaction.credit', {
          data: JSON.stringify({
            bank,
            provider: bank === 'KienlongBank' ? 'PAYOS' : undefined,
            orderCode: bank === 'KienlongBank' ? '100000778899' : undefined,
            transactionId: `tx_catch_up_${index}`,
            transactionNumber: '778899',
            credit: '250000',
            debit: '0',
            currency: 'VND',
            transactionDate: new Date().toISOString(),
            source: 'CATCH_UP',
            description: `LỊCH SỬ ${bank} KHÔNG ĐỌC`,
            detectedAt: new Date().toISOString(),
          }),
          lastEventId: `ep1:${888004 + index}`,
        }));
      }
    });
    await expect.poll(() => historyRequests).toBeGreaterThan(requestsBeforeCatchUp);
    expect(await spokenAnnouncements(page)).toHaveLength(1);
    await expect(history.getByText('LỊCH SỬ ACB KHÔNG ĐỌC', { exact: true })).toBeVisible();
    await expect(history.getByText('LỊCH SỬ KienlongBank KHÔNG ĐỌC', { exact: true })).toBeVisible();

    // A different committed order for the same amount must still be announced.
    const requestsBeforeSecondOrder = historyRequests;
    await page.evaluate(() => {
      // Installed by the EventSource fixture in beforeEach.
      const testWindow = window as VoiceFixtureWindow;
      const es = testWindow.__activeEventSource;
      if (!es) throw new Error('EventSource not initialized');
      es.dispatchEvent(new MessageEvent('bank.transaction.credit', {
        data: JSON.stringify({
          bank: 'KienlongBank', provider: 'PAYOS', orderCode: '100000556678',
          transactionId: 'tx_e2e_second_500k', transactionNumber: '556678',
          credit: '500000', debit: '0', currency: 'VND', source: 'REALTIME',
          transactionDate: new Date().toISOString(), detectedAt: new Date().toISOString(),
          description: 'KHÁCH THỨ HAI CÙNG SỐ TIỀN',
        }),
        lastEventId: 'ep1:888006',
      }));
    });
    await expect.poll(async () => (await spokenAnnouncements(page)).map(utterance => utterance.text))
      .toEqual([expect.stringContaining('năm trăm nghìn đồng'), expect.stringContaining('năm trăm nghìn đồng')]);
    await expect.poll(() => historyRequests).toBeGreaterThan(requestsBeforeSecondOrder);
    const announcements = await spokenAnnouncements(page);
    expect(announcements).toHaveLength(2);
    for (const announcement of announcements) {
      expect(announcement.text.match(/năm trăm nghìn đồng/g)).toHaveLength(1);
      expect(announcement.lang).toBe('vi-VN');
    }
    await expect(history.getByText('KHÁCH THỨ HAI CÙNG SỐ TIỀN', { exact: true })).toBeVisible();
  });

  test('unsaved template draft does not override confirmed profile', async ({ page }) => {
    const confirmedTemplate = 'Cấu hình đã lưu: {amount}.';
    const draftTemplate = 'Bản nháp chưa lưu: {amount}.';
    await page.route(/\/api\/(?:public\/)?v1\/voice\/profile$/, route => route.fulfill({
      json: { ...DEFAULT_VOICE_PROFILE, providerMode: 'BROWSER_ONLY', synthesisKey: 'a'.repeat(64),
        chimeEnabled: false, funEnabled: true, customTemplate: confirmedTemplate,
        amountBands: DEFAULT_VOICE_PROFILE.amountBands.map(band => ({ ...band, styleId: 'custom' })) },
    }));
    await page.route(/\/api\/v1\/status$/, async route => {
      const response = await route.fetch();
      await route.fulfill({ response, json: { ...await response.json(), userRole: 'OWNER' } });
    });
    const saves: string[] = [];
    page.on('request', request => {
      if (new URL(request.url()).pathname === '/api/v1/voice/settings' && request.method() === 'PUT') saves.push(request.url());
    });

    await page.goto('/transactions');
    await page.getByRole('button', { name: 'Tùy chỉnh giọng đọc' }).click();
    const templateInput = page.getByPlaceholder('Ví dụ: Đã nhận {amount} từ khách quý.');
    await expect(templateInput).toHaveValue(confirmedTemplate);
    await templateInput.fill(draftTemplate);
    await page.getByRole('button', { name: 'Nghe thử' }).click();

    const announcement = await expectTestAmount(page);
    expect(announcement.text).toContain('Cấu hình đã lưu:');
    expect(announcement.text).not.toContain('Bản nháp chưa lưu:');
    await expect(templateInput).toHaveValue(draftTemplate);
    expect(saves).toEqual([]);
  });
});
