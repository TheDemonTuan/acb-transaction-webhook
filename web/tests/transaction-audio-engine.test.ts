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

  it('in public mode: safely falls back to online TTS on Nghe thử (isTest) when no local Vietnamese voice installed', async () => {
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
      isTest: true,
      rate: 1.25,
      pitch: 1.0,
      template: 'Đa tạ quý khách vì {amount}.',
    });

    expect(mockSynth.speak).not.toHaveBeenCalled();
    expect(apiAudioSpy).toHaveBeenCalledTimes(1);
    expect(apiAudioSpy).toHaveBeenCalledWith(
      '/voice/test',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({
          voiceId: 'vi-VN-HoaiMyNeural',
          rate: 1.25,
          pitch: 1.0,
          template: 'Đa tạ quý khách vì {amount}.',
          includeDescription: undefined,
        }),
      })
    );
    expect(playBufferCalled).toBe(true);
  });

  it('in public mode with Audio constructor: progressively streams audio via HTMLAudioElement without full buffer download', async () => {
    let playCalled = false;
    let playingListener: any = null;
    let endedListener: any = null;
    let constructedUrl = '';

    class MockAudio {
      src: string;
      volume = 1;
      constructor(src: string) {
        this.src = src;
        constructedUrl = src;
      }
      addEventListener(event: string, cb: any) {
        if (event === 'playing') playingListener = cb;
        if (event === 'ended') endedListener = cb;
      }
      removeEventListener() {}
      play() {
        playCalled = true;
        setTimeout(() => {
          playingListener?.();
          setTimeout(() => {
            endedListener?.();
          }, 10);
        }, 10);
        return Promise.resolve();
      }
      pause() {}
      removeAttribute() {}
      load() {}
    }

    const mockSynth = {
      paused: false,
      speaking: false,
      getVoices: () => [],
      speak: vi.fn(),
      cancel: vi.fn(),
    };

    (globalThis as any).window = {
      Audio: MockAudio,
      speechSynthesis: mockSynth,
      SpeechSynthesisUtterance: class {},
    };

    const apiAudioSpy = vi.spyOn(apiModule, 'apiAudio');
    const engine = new TransactionAudioEngine({ isPublic: true });

    let startedTelemetry: any = null;
    await engine.speak({
      text: 'Đa tạ quý khách vì năm trăm nghìn đồng.',
      transactionId: 'tx_progressive_stream',
      rate: 1.25,
      pitch: 1.0,
      telemetry: {
        detectedAt: '2026-09-19T20:00:00.000Z',
        sseReceivedAt: Date.now() - 50,
      },
      onStart: (t) => {
        startedTelemetry = t;
      },
    });

    expect(playCalled).toBe(true);
    expect(constructedUrl).toContain('/api/public/v1/voice/transactions/tx_progressive_stream/stream');
    expect(constructedUrl).toContain('rate=1.25');
    expect(apiAudioSpy).not.toHaveBeenCalled();
    expect(startedTelemetry).not.toBeNull();
    expect(startedTelemetry.speechStartedAt).toBeGreaterThan(0);
  });

  it('duplicate playing events invoke onStart and telemetry exactly once', async () => {
    let playingListener: any = null;
    let endedListener: any = null;

    class MockAudio {
      src: string;
      volume = 1;
      constructor(src: string) {
        this.src = src;
      }
      addEventListener(event: string, cb: any) {
        if (event === 'playing') playingListener = cb;
        if (event === 'ended') endedListener = cb;
      }
      removeEventListener() {}
      play() {
        setTimeout(() => {
          playingListener?.();
          // Duplicate playing event
          playingListener?.();
          setTimeout(() => {
            endedListener?.();
          }, 10);
        }, 10);
        return Promise.resolve();
      }
      pause() {}
      removeAttribute() {}
      load() {}
    }

    (globalThis as any).window = {
      Audio: MockAudio,
      speechSynthesis: { getVoices: () => [], speak: vi.fn(), cancel: vi.fn() },
      SpeechSynthesisUtterance: class {},
    };

    const onStartSpy = vi.fn();
    const engine = new TransactionAudioEngine({ isPublic: true });

    await engine.speak({
      text: 'Thử nghiệm duplicate playing',
      transactionId: 'tx_duplicate_playing',
      onStart: onStartSpy,
    });

    expect(onStartSpy).toHaveBeenCalledTimes(1);
  });

  it('initial failure prior to playing event falls back to buffered audio', async () => {
    let errorListener: any = null;

    class FailingAudio {
      src: string;
      volume = 1;
      constructor(src: string) {
        this.src = src;
      }
      addEventListener(event: string, cb: any) {
        if (event === 'error') errorListener = cb;
      }
      removeEventListener() {}
      play() {
        setTimeout(() => {
          errorListener?.();
        }, 10);
        return Promise.resolve();
      }
      pause() {}
      removeAttribute() {}
      load() {}
    }

    class MockAudioContext {
      state = 'running';
      destination = {};
      createGain() {
        return { connect: vi.fn(), gain: { setValueAtTime: vi.fn() } };
      }
      createBufferSource() {
        return {
          buffer: null,
          connect: vi.fn(),
          start: vi.fn(function (this: any) {
            setTimeout(() => this.onended?.(), 10);
          }),
        };
      }
      decodeAudioData() {
        return Promise.resolve({ duration: 1.0 });
      }
      resume = vi.fn().mockResolvedValue(undefined);
    }

    (globalThis as any).window = {
      Audio: FailingAudio,
      AudioContext: MockAudioContext,
      speechSynthesis: { getVoices: () => [], speak: vi.fn(), cancel: vi.fn() },
      SpeechSynthesisUtterance: class {},
    };

    const apiAudioSpy = vi.spyOn(apiModule, 'apiAudio').mockResolvedValue({
      data: new ArrayBuffer(8),
      provider: 'edge',
      voice: 'vi-VN-HoaiMyNeural',
      fallback: false,
      cached: true,
    });

    const engine = new TransactionAudioEngine({ isPublic: false });

    await engine.speak({
      text: 'Test fallback on initial error',
      transactionId: 'tx_initial_fail',
    });

    // Failing prior to playback starts must invoke buffered fallback
    expect(apiAudioSpy).toHaveBeenCalledTimes(1);
    expect(apiAudioSpy).toHaveBeenCalledWith(
      '/voice/transactions/tx_initial_fail',
      expect.anything()
    );
  });

  it('mid-stream failure after playing event terminates without falling back to buffered replay', async () => {
    let playingListener: any = null;
    let errorListener: any = null;

    class MidstreamFailingAudio {
      src: string;
      volume = 1;
      constructor(src: string) {
        this.src = src;
      }
      addEventListener(event: string, cb: any) {
        if (event === 'playing') playingListener = cb;
        if (event === 'error') errorListener = cb;
      }
      removeEventListener() {}
      play() {
        setTimeout(() => {
          // First starts playing
          playingListener?.();
          setTimeout(() => {
            // Then mid-stream error occurs
            errorListener?.();
          }, 10);
        }, 10);
        return Promise.resolve();
      }
      pause() {}
      removeAttribute() {}
      load() {}
    }

    (globalThis as any).window = {
      Audio: MidstreamFailingAudio,
      speechSynthesis: { getVoices: () => [], speak: vi.fn(), cancel: vi.fn() },
      SpeechSynthesisUtterance: class {},
    };

    const apiAudioSpy = vi.spyOn(apiModule, 'apiAudio');
    const engine = new TransactionAudioEngine({ isPublic: false });

    await engine.speak({
      text: 'Test no duplicate on midstream error',
      transactionId: 'tx_midstream_fail',
    });

    // Mid-stream failure must NOT fall back to buffered replay (prevents hearing audio twice)
    expect(apiAudioSpy).not.toHaveBeenCalled();
  });

  it('summary audio requests populate streamUrl with query parameters', async () => {
    let capturedStreamUrl = '';
    let playingListener: any = null;
    let endedListener: any = null;

    class MockAudio {
      src: string;
      volume = 1;
      constructor(src: string) {
        this.src = src;
        capturedStreamUrl = src;
      }
      addEventListener(event: string, cb: any) {
        if (event === 'playing') playingListener = cb;
        if (event === 'ended') endedListener = cb;
      }
      removeEventListener() {}
      play() {
        setTimeout(() => {
          playingListener?.();
          setTimeout(() => endedListener?.(), 10);
        }, 10);
        return Promise.resolve();
      }
      pause() {}
      removeAttribute() {}
      load() {}
    }

    (globalThis as any).window = {
      Audio: MockAudio,
      speechSynthesis: { getVoices: () => [], speak: vi.fn(), cancel: vi.fn() },
      SpeechSynthesisUtterance: class {},
    };

    const apiAudioSpy = vi.spyOn(apiModule, 'apiAudio');
    const engine = new TransactionAudioEngine({ isPublic: false });

    await engine.speak({
      text: 'Phát tổng hợp 2 giao dịch',
      summaryTransactionIds: ['tx_sum_1', 'tx_sum_2'],
      rate: 1.2,
      pitch: 1.0,
    });

    expect(capturedStreamUrl).toContain('/api/v1/voice/transactions/summary/stream');
    expect(capturedStreamUrl).toContain('transactionIds=tx_sum_1%2Ctx_sum_2');
    expect(apiAudioSpy).not.toHaveBeenCalled();
  });
});
