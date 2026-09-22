import { apiAudio } from '../../api';
import { isPublicViewerHost } from '../../app/runtime-mode';
import { BrowserSpeechEngine } from './browser-speech-engine';
import type { VoiceEngine, VoiceInfo, VoiceMessage } from './voice-engine';

export interface TransactionAudioEngineOptions {
  isPublic?: boolean;
}

export class TransactionAudioEngine implements VoiceEngine {
  private audioContext: AudioContext | null = null;
  private gainNode: GainNode | null = null;
  private currentSource: AudioBufferSourceNode | null = null;
  private currentAudioElement: HTMLAudioElement | null = null;
  private browserFallback = new BrowserSpeechEngine();
  private abortController: AbortController | null = null;
  private isPublic: boolean;

  constructor(options?: TransactionAudioEngineOptions) {
    this.isPublic = options?.isPublic ?? (typeof window !== 'undefined' && isPublicViewerHost());
  }

  public isSupported(): boolean {
    return (
      typeof window !== 'undefined' &&
      (typeof window.AudioContext !== 'undefined' ||
        typeof (window as any).webkitAudioContext !== 'undefined')
    );
  }

  private initAudioContext(): AudioContext | null {
    if (this.audioContext) return this.audioContext;
    if (typeof window === 'undefined') return null;

    const AudioContextClass =
      window.AudioContext || (window as any).webkitAudioContext;
    if (!AudioContextClass) return null;

    try {
      this.audioContext = new AudioContextClass();
      this.gainNode = this.audioContext.createGain();
      this.gainNode.connect(this.audioContext.destination);
      return this.audioContext;
    } catch {
      return null;
    }
  }

  public async unlock(): Promise<boolean> {
    const ctx = this.initAudioContext();
    if (!ctx) return false;
    if (ctx.state === 'suspended') {
      try {
        await ctx.resume();
      } catch {
        return false;
      }
    }
    return ctx.state === 'running';
  }

  public async getVoices(): Promise<VoiceInfo[]> {
    const onlineVoices: VoiceInfo[] = [
      {
        uri: 'vi-VN-HoaiMyNeural',
        name: 'Hoài My (Giọng trực tuyến)',
        lang: 'vi-VN',
        isDefault: true,
      },
      {
        uri: 'vi-VN-NamMinhNeural',
        name: 'Nam Minh (Giọng trực tuyến)',
        lang: 'vi-VN',
        isDefault: false,
      },
    ];

    try {
      const localVi = await this.browserFallback.getVoices();
      return [...onlineVoices, ...localVi];
    } catch {
      return onlineVoices;
    }
  }

  public async speak(message: VoiceMessage): Promise<void> {
    this.cancel();

    if (this.isPublic) {
      // Public hybrid strategy: local Vietnamese voice first, fallback to safe transaction-scoped or test online TTS
      const isOnlineVoiceChosen = Boolean(message.voiceURI?.startsWith('vi-VN-'));
      if (isOnlineVoiceChosen && (message.transactionId || message.isTest)) {
        try {
          await this.speakOnline(message);
          return;
        } catch (onlineErr: any) {
          if (
            (onlineErr instanceof DOMException && onlineErr.name === 'AbortError') ||
            onlineErr?.name === 'AbortError' ||
            onlineErr?.message === 'VOICE_CANCELLED'
          ) {
            throw new Error('VOICE_CANCELLED');
          }
          if (onlineErr?.playbackStarted || onlineErr?.message?.startsWith('AUDIO_STREAM_INTERRUPTED')) {
            return;
          }
          await this.browserFallback.speak(message);
          return;
        }
      }

      // Try local browser voice first
      try {
        await this.browserFallback.speak(message);
        return;
      } catch (browserError: any) {
        if (
          (browserError instanceof DOMException && browserError.name === 'AbortError') ||
          browserError?.name === 'AbortError' ||
          browserError?.message === 'VOICE_CANCELLED'
        ) {
          throw new Error('VOICE_CANCELLED');
        }
        // Fallback to online transaction or test TTS if transactionId present or isTest
        if (message.transactionId || message.isTest) {
          try {
            await this.speakOnline(message);
            return;
          } catch (onlineError: any) {
            if (
              (onlineError instanceof DOMException && onlineError.name === 'AbortError') ||
              onlineError?.name === 'AbortError' ||
              onlineError?.message === 'VOICE_CANCELLED'
            ) {
              throw new Error('VOICE_CANCELLED');
            }
            if (onlineError?.playbackStarted || onlineError?.message?.startsWith('AUDIO_STREAM_INTERRUPTED')) {
              return;
            }
            throw browserError;
          }
        }
        throw browserError;
      }
    }

    // Admin strategy: Try online transaction audio synthesis first
    try {
      await this.speakOnline(message);
      return;
    } catch (onlineError: any) {
      if (
        (onlineError instanceof DOMException && onlineError.name === 'AbortError') ||
        onlineError?.name === 'AbortError' ||
        onlineError?.message === 'VOICE_CANCELLED'
      ) {
        throw new Error('VOICE_CANCELLED');
      }
      if (onlineError?.playbackStarted || onlineError?.message?.startsWith('AUDIO_STREAM_INTERRUPTED')) {
        return;
      }
      // Fallback to BrowserSpeechEngine with strict Vietnamese voice ONLY if not cancelled
      try {
        await this.browserFallback.speak(message);
      } catch (browserError) {
        throw browserError;
      }
    }
  }

  private async speakOnline(message: VoiceMessage): Promise<void> {
    this.abortController = new AbortController();
    const signal = this.abortController.signal;

    let path = '';
    let body: any = null;
    let streamUrl: string | null = null;
    const basePath = this.isPublic ? '/api/public/v1' : '/api/v1';

    const searchParams = new URLSearchParams();
    if (message.voiceURI) searchParams.set('voiceId', message.voiceURI);
    if (message.rate !== undefined) searchParams.set('rate', String(message.rate));
    if (message.pitch !== undefined) searchParams.set('pitch', String(message.pitch));
    if (message.template) searchParams.set('template', message.template);
    if (message.includeDescription) searchParams.set('includeDescription', 'true');
    const querySuffix = searchParams.toString() ? `?${searchParams.toString()}` : '';

    if (message.isTest) {
      path = '/voice/test';
      streamUrl = `${basePath}/voice/test/stream${querySuffix}`;
      body = {
        voiceId: message.voiceURI?.startsWith('vi-VN-') ? message.voiceURI : 'vi-VN-HoaiMyNeural',
        rate: message.rate,
        pitch: message.pitch,
        template: message.template,
        includeDescription: message.includeDescription,
      };
    } else if (message.isReplay && message.transactionId) {
      path = `/voice/transactions/${encodeURIComponent(message.transactionId)}/replay`;
      streamUrl = `${basePath}/voice/transactions/${encodeURIComponent(message.transactionId)}/replay/stream${querySuffix}`;
      body = {
        includeDescription: Boolean(message.includeDescription),
        template: message.template,
        voiceId: message.voiceURI,
        rate: message.rate,
        pitch: message.pitch,
      };
    } else if (
      message.summaryTransactionIds &&
      message.summaryTransactionIds.length >= 2
    ) {
      path = '/voice/transactions/summary';
      const summaryParams = new URLSearchParams(searchParams);
      summaryParams.set('transactionIds', message.summaryTransactionIds.join(','));
      streamUrl = `${basePath}/voice/transactions/summary/stream?${summaryParams.toString()}`;
      body = {
        transactionIds: message.summaryTransactionIds,
        includeDescription: false,
        voiceId: message.voiceURI,
        rate: message.rate,
        pitch: message.pitch,
      };
    } else if (message.transactionId) {
      path = `/voice/transactions/${encodeURIComponent(message.transactionId)}`;
      streamUrl = `${basePath}/voice/transactions/${encodeURIComponent(message.transactionId)}/stream${querySuffix}`;
      body = {
        includeDescription: Boolean(message.includeDescription),
        template: message.template,
        voiceId: message.voiceURI,
        rate: message.rate,
        pitch: message.pitch,
      };
    } else {
      // If no transaction ID, fall back directly to browser speech
      throw new Error('NO_ONLINE_ROUTE');
    }

    const AudioCtor = (typeof window !== 'undefined' && (window as any).Audio) || (typeof Audio !== 'undefined' ? Audio : null);
    if (streamUrl && AudioCtor) {
      try {
        await this.playAudioStream(streamUrl, message.volume ?? 1, message, signal, AudioCtor);
        return;
      } catch (streamErr: any) {
        if (
          (streamErr instanceof DOMException && streamErr.name === 'AbortError') ||
          streamErr?.name === 'AbortError' ||
          streamErr?.message === 'VOICE_CANCELLED'
        ) {
          throw new Error('VOICE_CANCELLED');
        }
        if (streamErr?.playbackStarted || streamErr?.message?.startsWith('AUDIO_STREAM_INTERRUPTED')) {
          throw streamErr;
        }
        // Fallback to arrayBuffer decoding if stream playback fails before playback starts
      }
    }

    const { data: audioData } = await apiAudio(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal,
    });

    if (signal.aborted) {
      throw new Error('VOICE_CANCELLED');
    }

    await this.playAudioBuffer(audioData, message.volume ?? 1, message);
  }

  private playAudioStream(
    streamUrl: string,
    volume: number,
    message: VoiceMessage | undefined,
    signal: AbortSignal | undefined,
    AudioCtor: any
  ): Promise<void> {
    return new Promise((resolve, reject) => {
      let audio: HTMLAudioElement;
      try {
        audio = new AudioCtor(streamUrl);
      } catch (err) {
        reject(err);
        return;
      }
      this.currentAudioElement = audio;
      audio.volume = Math.max(0, Math.min(1, volume));

      let settled = false;
      let playbackStarted = false;
      let startedOnce = false;

      const createError = (baseMsg: string) => {
        const err: any = new Error(baseMsg);
        err.playbackStarted = playbackStarted;
        return err;
      };

      const cleanup = () => {
        if (this.currentAudioElement === audio) {
          this.currentAudioElement = null;
        }
        signal?.removeEventListener('abort', onAbort);
      };

      const onAbort = () => {
        if (!settled) {
          settled = true;
          try {
            audio.pause();
            audio.removeAttribute('src');
            audio.load?.();
          } catch {}
          cleanup();
          reject(new Error('VOICE_CANCELLED'));
        }
      };

      if (signal?.aborted) {
        onAbort();
        return;
      }
      signal?.addEventListener('abort', onAbort);

      audio.addEventListener?.('playing', () => {
        playbackStarted = true;
        if (startedOnce) return;
        startedOnce = true;

        if (message?.telemetry) {
          message.telemetry.speechStartedAt = Date.now();
          const sseAt = message.telemetry.sseReceivedAt || message.telemetry.queuedAt;
          if (sseAt) {
            message.telemetry.sseToSpeakMs = message.telemetry.speechStartedAt - sseAt;
          }
          if (message.telemetry.queuedAt) {
            message.telemetry.queueToSpeakMs = message.telemetry.speechStartedAt - message.telemetry.queuedAt;
          }
          if (message.telemetry.detectedAt) {
            const d = new Date(message.telemetry.detectedAt).getTime();
            if (!isNaN(d)) {
              message.telemetry.totalLatencyMs = message.telemetry.speechStartedAt - d;
            }
          }
        }
        message?.onStart?.(message.telemetry);
      });

      audio.addEventListener?.('ended', () => {
        if (!settled) {
          settled = true;
          cleanup();
          resolve();
        }
      });

      audio.addEventListener?.('error', () => {
        if (!settled) {
          settled = true;
          cleanup();
          const msg = playbackStarted
            ? `AUDIO_STREAM_INTERRUPTED: ${audio.error?.message || 'unknown'}`
            : `AUDIO_PLAYBACK_ERROR: ${audio.error?.message || 'unknown'}`;
          reject(createError(msg));
        }
      });

      try {
        const playPromise = audio.play();
        if (playPromise && typeof playPromise.then === 'function') {
          playPromise.catch((err: any) => {
            if (!settled) {
              settled = true;
              cleanup();
              if (err && typeof err === 'object') {
                err.playbackStarted = playbackStarted;
              }
              reject(err);
            }
          });
        }
      } catch (err: any) {
        if (!settled) {
          settled = true;
          cleanup();
          if (err && typeof err === 'object') {
            err.playbackStarted = playbackStarted;
          }
          reject(err);
        }
      }
    });
  }

  private async playAudioBuffer(
    arrayBuffer: ArrayBuffer,
    volume: number,
    message?: VoiceMessage
  ): Promise<void> {
    const ctx = this.initAudioContext();
    if (!ctx) {
      throw new Error('AUDIO_CONTEXT_UNAVAILABLE');
    }

    if (ctx.state === 'suspended') {
      try {
        await ctx.resume();
      } catch {
        throw new Error('AUDIO_LOCKED');
      }
    }

    const audioBuffer = await ctx.decodeAudioData(arrayBuffer);

    return new Promise((resolve, reject) => {
      try {
        const source = ctx.createBufferSource();
        source.buffer = audioBuffer;

        if (this.gainNode) {
          this.gainNode.gain.setValueAtTime(Math.max(0, Math.min(1, volume)), ctx.currentTime);
          source.connect(this.gainNode);
        } else {
          source.connect(ctx.destination);
        }

        let settled = false;

        source.onended = () => {
          if (!settled) {
            settled = true;
            this.currentSource = null;
            resolve();
          }
        };

        this.currentSource = source;
        if (message?.telemetry) {
          message.telemetry.speechStartedAt = Date.now();
          const sseAt = message.telemetry.sseReceivedAt || message.telemetry.queuedAt;
          if (sseAt) {
            message.telemetry.sseToSpeakMs = message.telemetry.speechStartedAt - sseAt;
          }
          if (message.telemetry.queuedAt) {
            message.telemetry.queueToSpeakMs = message.telemetry.speechStartedAt - message.telemetry.queuedAt;
          }
          if (message.telemetry.detectedAt) {
            const d = new Date(message.telemetry.detectedAt).getTime();
            if (!isNaN(d)) {
              message.telemetry.totalLatencyMs = message.telemetry.speechStartedAt - d;
            }
          }
        }
        message?.onStart?.(message.telemetry);
        source.start(0);
      } catch (err) {
        reject(err);
      }
    });
  }

  public cancel(): void {
    if (this.abortController) {
      this.abortController.abort();
      this.abortController = null;
    }
    if (this.currentAudioElement) {
      try {
        this.currentAudioElement.pause();
        this.currentAudioElement.removeAttribute('src');
        this.currentAudioElement.load?.();
      } catch {}
      this.currentAudioElement = null;
    }
    if (this.currentSource) {
      try {
        this.currentSource.stop();
      } catch {}
      this.currentSource = null;
    }
    this.browserFallback.cancel();
  }
}
