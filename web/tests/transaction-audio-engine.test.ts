import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TransactionAudioEngine } from '../src/features/voice-announcements/transaction-audio-engine';
import * as apiModule from '../src/api';

describe('TransactionAudioEngine public fallback & template propagation', () => {
  let prevWindow: any;

  beforeEach(() => {
    prevWindow = (globalThis as any).window;
  });

  afterEach(() => {
    (globalThis as any).window = prevWindow;
    vi.restoreAllMocks();
  });

  it('in public mode: uses local Vietnamese voice first when available', async () => {
    class MockUtterance {
      text: string;
      lang = 'vi-VN';
      voice: any = null;
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
        }, 10);
      }),
      cancel: vi.fn(),
      resume: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    };

    (globalThis as any).window = {
      speechSynthesis: mockSynth,
      SpeechSynthesisUtterance: MockUtterance,
      AudioContext: class {
        state = 'running';
        destination = {};
        createGain() {
          return { connect: vi.fn() };
        }
        resume = vi.fn().mockResolvedValue(undefined);
      },
    };

    const apiAudioSpy = vi.spyOn(apiModule, 'apiAudio');
    const engine = new TransactionAudioEngine({ isPublic: true });

    await engine.speak({
      text: 'Đa tạ quý khách vì năm trăm nghìn đồng.',
      transactionId: 'tx_local_first',
      rate: 1.25,
    });

    expect(mockSynth.speak).toHaveBeenCalledTimes(1);
    expect(apiAudioSpy).not.toHaveBeenCalled();
  });

  it('in public mode: safely falls back to online TTS when no local Vietnamese voice installed', async () => {
    class MockUtterance {
      text: string;
      constructor(text: string) {
        this.text = text;
      }
    }

    const mockSynth = {
      paused: false,
      speaking: false,
      getVoices: () => [
        // Only English voice installed
        {
          default: true,
          lang: 'en-US',
          name: 'Microsoft David',
          voiceURI: 'en-us-david',
        },
      ],
      speak: vi.fn(),
      cancel: vi.fn(),
      resume: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    };

    let playBufferCalled = false;
    class MockAudioBufferSource {
      buffer: any = null;
      onended: any = null;
      connect = vi.fn();
      start = vi.fn(() => {
        playBufferCalled = true;
        setTimeout(() => this.onended?.(), 10);
      });
    }

    class MockAudioContext {
      state = 'running';
      currentTime = 0;
      destination = {};
      createGain() {
        return {
          connect: vi.fn(),
          gain: { setValueAtTime: vi.fn() },
        };
      }
      createBufferSource() {
        return new MockAudioBufferSource();
      }
      decodeAudioData() {
        return Promise.resolve({ duration: 1.5 });
      }
      resume = vi.fn().mockResolvedValue(undefined);
    }

    (globalThis as any).window = {
      speechSynthesis: mockSynth,
      SpeechSynthesisUtterance: MockUtterance,
      AudioContext: MockAudioContext,
    };

    const apiAudioSpy = vi.spyOn(apiModule, 'apiAudio').mockResolvedValue({
      data: new ArrayBuffer(8),
      provider: 'edge',
      voice: 'vi-VN-HoaiMyNeural',
      fallback: false,
      cached: true,
    });

    const engine = new TransactionAudioEngine({ isPublic: true });

    await engine.speak({
      text: 'Đa tạ quý khách vì năm trăm nghìn đồng.',
      transactionId: 'tx_online_fallback_123',
      rate: 1.25,
      pitch: 1.0,
      template: 'Đa tạ quý khách vì {amount}.',
    });

    expect(mockSynth.speak).not.toHaveBeenCalled();
    expect(apiAudioSpy).toHaveBeenCalledTimes(1);
    expect(apiAudioSpy).toHaveBeenCalledWith(
      '/voice/transactions/tx_online_fallback_123',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({
          includeDescription: false,
          template: 'Đa tạ quý khách vì {amount}.',
          voiceId: undefined,
          rate: 1.25,
          pitch: 1.0,
        }),
      })
    );
    expect(playBufferCalled).toBe(true);
  });
});
