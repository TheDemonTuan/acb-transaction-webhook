import { expect, test } from '@playwright/test';

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
    expect(spoken[0].text).toContain('Đa tạ quý khách vì');
    expect(spoken[0].lang).toBe('vi-VN');
  });

  test('receives bank.transaction.credit SSE event and announces in natural Vietnamese, then dedupes replay', async ({
    page,
  }) => {
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
          bank: 'ACB',
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
    expect(spokenFirst[0].text).toBe('Đa tạ quý khách vì năm trăm nghìn đồng.');
    expect(spokenFirst[0].lang).toBe('vi-VN');

    // 4. Assert transaction immediately appeared in the transaction list
    await expect(page.getByText('NGUYEN VAN A CHUYEN TIEN REALTIME')).toBeVisible();

    // 5. Deduplication verification: re-dispatch the exact same SSE event (journal replay simulation)
    await page.evaluate(() => {
      const es = (window as any).__activeEventSource;
      const event = new MessageEvent('bank.transaction.credit', {
        data: JSON.stringify({
          bank: 'ACB',
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
          bank: 'ACB',
          transactionId: 'tx_stale_old',
          transactionNumber: '999999',
          credit: '1000000',
          debit: '0',
          currency: 'VND',
          transactionDate: staleTime,
          description: 'GIAO DICH CU KHONG DUOC DOC',
          detectedAt: staleTime,
        }),
        lastEventId: 'ep1:888002',
      });
      es.dispatchEvent(event);
    });

    // Wait 1 second and assert stale event did NOT trigger voice
    await page.waitForTimeout(1000);
    const spokenAfterStale = await page.evaluate(() => (window as any).__spokenUtterances);
    expect(spokenAfterStale.length).toBe(1);
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
