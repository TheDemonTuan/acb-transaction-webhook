import { expect, test } from '@playwright/test';

type VoiceFixtureWindow = Window & {
  __activeEventSource?: EventSource;
  __spokenUtterances: Array<{ text: string; lang: string; volume: number; rate: number }>;
};

test.describe('Voice Announcements & Realtime Features', () => {
  test.beforeEach(async ({ page }) => {
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
          setTimeout(() => {
            utterance.onend?.(new Event('end'));
          }, 40);
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
    await expect(page.getByRole('heading', { name: 'Giao dịch' })).toBeVisible();

    const voiceSettingsBtn = page.getByRole('button', { name: 'Tùy chỉnh giọng đọc' });
    await expect(voiceSettingsBtn).toBeVisible();
    await voiceSettingsBtn.click();

    await expect(page.getByRole('heading', { name: 'Cài đặt đọc giao dịch' })).toBeVisible();
    await expect(page.getByText('Tự động đọc số tiền ngay khi giao dịch được ghi nhận')).toBeVisible();

    const testBtn = page.getByRole('button', { name: 'Nghe thử' });
    await expect(testBtn).toBeVisible();
    await testBtn.click({ force: true });

    await page.waitForFunction(() => (window as any).__spokenUtterances.length > 0);
    const spoken = await page.evaluate(() => (window as any).__spokenUtterances);
    expect(spoken.length).toBeGreaterThanOrEqual(1);
    expect(spoken[0].text).toContain('năm trăm nghìn đồng');
    expect(spoken[0].lang).toBe('vi-VN');
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
    await page.getByRole('switch', { name: 'Bật đọc giao dịch' }).click({ force: true });
    await page.getByRole('button', { name: 'Đóng' }).click({ force: true });
    await expect(page.getByRole('heading', { name: 'Cài đặt đọc giao dịch' })).toHaveCount(0);

    // Clear test utterances
    await page.evaluate(() => {
      (window as any).__spokenUtterances = [];
    });

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

    // 3. Assert speechSynthesis spoke the natural Vietnamese phrase after burst collection
    await page.waitForFunction(
      () =>
        (window as any).__spokenUtterances.length > 0 &&
        (window as any).__spokenUtterances.some((u: any) =>
          u.text.includes('năm trăm nghìn đồng')
        ),
      { timeout: 5000 }
    );

    const spokenFirst = await page.evaluate(() => (window as any).__spokenUtterances);
    expect(spokenFirst.length).toBe(1);
    expect(spokenFirst[0].text.match(/năm trăm nghìn đồng/g)).toHaveLength(1);
    expect(spokenFirst[0].lang).toBe('vi-VN');

    // 4. Assert transaction immediately appeared in the transaction list
    await expect(page.getByText('NGUYEN VAN A CHUYEN TIEN REALTIME')).toBeVisible();
    await expect(page.getByText('KienlongBank', { exact: true }).first()).toBeVisible();
    await expect(page.getByText('ACB', { exact: true }).first()).toBeVisible();
    expect(historyRequests).toBeGreaterThanOrEqual(2);

    // 5. Replay the same committed transaction under a different SSE event ID.
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

    // Wait 1 second and assert speech count stayed at 1 (no duplicate announcement!)
    await page.waitForTimeout(1000);
    const spokenAfterReplay = await page.evaluate(() => (window as any).__spokenUtterances);
    expect(spokenAfterReplay.length).toBe(1);

    // 6. Stale event suppression verification: event older than 150 seconds should be ignored for speech
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

    // Wait 1 second and assert stale event did NOT trigger voice
    await page.waitForTimeout(1000);
    const spokenAfterStale = await page.evaluate(() => (window as any).__spokenUtterances);
    expect(spokenAfterStale.length).toBe(1);

    // Historical ACB and recovered payOS credits update history, never speech.
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
    await page.waitForTimeout(1000);
    expect(await page.evaluate(() => {
      // Installed by the speech fixture in beforeEach.
      const testWindow = window as VoiceFixtureWindow;
      return testWindow.__spokenUtterances.length;
    })).toBe(1);
    await expect(page.getByText('LỊCH SỬ ACB KHÔNG ĐỌC')).toBeVisible();
    await expect(page.getByText('LỊCH SỬ KienlongBank KHÔNG ĐỌC')).toBeVisible();

    // A different committed order for the same amount must still be announced.
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
    await expect.poll(() => page.evaluate(() => {
      // Installed by the speech fixture in beforeEach.
      const testWindow = window as VoiceFixtureWindow;
      return testWindow.__spokenUtterances.length;
    })).toBe(2);
    await expect(page.getByText('KHÁCH THỨ HAI CÙNG SỐ TIỀN')).toBeVisible();
  });

  test('custom announcement template formats phrase correctly', async ({ page }) => {
    await page.goto('/transactions');
    await page.getByRole('button', { name: 'Tùy chỉnh giọng đọc' }).click();
    await expect(page.getByRole('heading', { name: 'Cài đặt đọc giao dịch' })).toBeVisible();

    const templateInput = page.getByPlaceholder('Ví dụ: Đa tạ quý khách vì {amount}.');
    if (await templateInput.isVisible()) {
      await templateInput.fill('Cảm ơn bạn đã gửi {amount}.');
    }

    const testBtn = page.getByRole('button', { name: 'Nghe thử' });
    await testBtn.click({ force: true });

    await page.waitForFunction(() => (window as any).__spokenUtterances.length > 0);
    const spoken = await page.evaluate(() => (window as any).__spokenUtterances);
    expect(spoken[spoken.length - 1].text).toContain('năm trăm nghìn đồng');
  });
});
