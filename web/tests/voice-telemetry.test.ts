import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { BrowserSpeechEngine } from '../src/features/voice-announcements/browser-speech-engine';
import type { VoiceMessage, VoiceTelemetry } from '../src/features/voice-announcements/voice-engine';

describe('voice-telemetry', () => {
  let prevWindow: any;

  beforeEach(() => {
    prevWindow = (globalThis as any).window;
  });

  afterEach(() => {
    (globalThis as any).window = prevWindow;
  });

  it('populates timestamp breakdown from detectedAt -> SSE -> queue -> speak onstart', async () => {
    class MockUtterance {
      text: string;
      lang = 'vi-VN';
      voice: any = null;
      rate = 1;
      onstart: any = null;
      onend: any = null;
      constructor(text: string) {
        this.text = text;
      }
    }

    const mockSynth = {
      paused: false,
      speaking: false,
      getVoices: () => [
        {
          default: true,
          lang: 'vi-VN',
          name: 'Microsoft Hoai My',
          voiceURI: 'vi-vn-hoaimy',
        },
      ],
      speak: vi.fn((utt: any) => {
        setTimeout(() => {
          utt.onstart?.(new Event('start'));
          utt.onend?.(new Event('end'));
        }, 20);
      }),
      cancel: vi.fn(),
      resume: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    };

    (globalThis as any).window = {
      speechSynthesis: mockSynth,
      SpeechSynthesisUtterance: MockUtterance,
    };

    const engine = new BrowserSpeechEngine();

    const detectedAt = new Date(Date.now() - 1200).toISOString();
    const sseReceivedAt = Date.now() - 50;
    const queuedAt = Date.now() - 20;

    const telemetry: VoiceTelemetry = {
      detectedAt,
      sseReceivedAt,
      queuedAt,
      detectedToSseMs: sseReceivedAt - new Date(detectedAt).getTime(),
      sseToQueueMs: queuedAt - sseReceivedAt,
    };

    let receivedTelemetry: VoiceTelemetry | undefined;
    const message: VoiceMessage = {
      text: 'Đa tạ quý khách vì mười nghìn đồng.',
      telemetry,
      onStart: (t) => {
        receivedTelemetry = t;
      },
    };

    await engine.speak(message);

    expect(receivedTelemetry).toBeDefined();
    expect(receivedTelemetry?.speechStartedAt).toBeGreaterThanOrEqual(queuedAt);
    expect(receivedTelemetry?.queueToSpeakMs).toBeGreaterThanOrEqual(0);
    expect(receivedTelemetry?.sseToSpeakMs).toBeGreaterThanOrEqual(0);
    expect(receivedTelemetry?.totalLatencyMs).toBeGreaterThan(1000);
  });
});
