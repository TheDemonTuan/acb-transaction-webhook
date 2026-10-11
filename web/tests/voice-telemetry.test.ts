import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { BrowserSpeechEngine } from '../src/features/voice-announcements/browser-speech-engine';
import type { VoiceMessage, VoiceTelemetry } from '../src/features/voice-announcements/voice-engine';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import { clearVoiceTelemetry, exportVoiceTelemetry, recordVoiceTelemetry } from '../src/features/voice-announcements/voice-telemetry';

interface GlobalWindowHolder {
  window?: unknown;
}

describe('voice-telemetry', () => {
  let prevWindow: unknown;

  beforeEach(() => {
    const holder = globalThis as unknown as GlobalWindowHolder;
    prevWindow = holder.window;
  });

  afterEach(() => {
    const holder = globalThis as unknown as GlobalWindowHolder;
    holder.window = prevWindow;
  });

  it('populates timestamp breakdown from detectedAt -> SSE -> queue -> speak onstart', async () => {
    class MockUtterance {
      text: string;
      lang = 'vi-VN';
      voice: SpeechSynthesisVoice | null = null;
      rate = 1;
      onstart: ((ev: Event) => void) | null = null;
      onend: ((ev: Event) => void) | null = null;
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
      speak: vi.fn((utt: unknown) => {
        if (utt && typeof utt === 'object' && 'onstart' in utt && 'onend' in utt) {
          const startCb = utt.onstart;
          const endCb = utt.onend;
          queueMicrotask(() => {
            if (typeof startCb === 'function') {
              startCb(new Event('start'));
            }
            if (typeof endCb === 'function') {
              endCb(new Event('end'));
            }
          });
        }
      }),
      cancel: vi.fn(),
      resume: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    };

    const holder = globalThis as unknown as GlobalWindowHolder;
    holder.window = {
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
      sseToQueueMs: queuedAt - sseReceivedAt,
      receivedMonotonicAt: performance.now() - 50,
    };

    let receivedTelemetry: VoiceTelemetry | undefined;
    const message: VoiceMessage = {
      text: 'Đa tạ quý khách vì mười nghìn đồng.',
      profile: { ...DEFAULT_VOICE_PROFILE, providerMode: 'BROWSER_ONLY' },
      voiceURI: 'vi-vn-hoaimy',
      queuedMonotonicAt: performance.now() - 20,
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
  });

  it('records telemetry in bounded ring buffer without PII', () => {
    clearVoiceTelemetry();
    const payloadWithSecret = {
      detectedAt: '2026-10-11T12:00:00Z',
      sseToSpeakMs: 320,
      audioPath: 'decoded-pack' as const,
      outcome: 'completed' as const,
      text: 'Secret text should be omitted',
    };
    recordVoiceTelemetry(payloadWithSecret);

    const exported = exportVoiceTelemetry();
    expect(exported.schemaVersion).toBe(1);
    expect(exported.samples).toHaveLength(1);
    const firstSample = exported.samples[0];
    expect(firstSample.sseToSpeakMs).toBe(320);
    expect(firstSample.audioPath).toBe('decoded-pack');
    expect(firstSample.outcome).toBe('completed');
    expect('text' in firstSample).toBe(false);
  });
});
