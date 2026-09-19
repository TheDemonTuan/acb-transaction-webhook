import type { VoiceEngine, VoiceInfo, VoiceMessage } from './voice-engine';

export type BrowserVoiceStatus =
  | 'supported'
  | 'no-vietnamese-voice'
  | 'locked'
  | 'ready'
  | 'error';

export function isVietnameseVoice(v: { lang?: string }): boolean {
  if (!v.lang) return false;
  const lang = v.lang.toLowerCase().replace('_', '-');
  return lang === 'vi' || lang.startsWith('vi-');
}

export class BrowserSpeechEngine implements VoiceEngine {
  private currentStatus: BrowserVoiceStatus = 'supported';
  private audioReady = false;

  public isSupported(): boolean {
    return (
      typeof window !== 'undefined' &&
      'speechSynthesis' in window &&
      'SpeechSynthesisUtterance' in window
    );
  }

  public getStatus(): BrowserVoiceStatus {
    if (!this.isSupported()) return 'error';
    return this.currentStatus;
  }

  public async checkStatus(): Promise<BrowserVoiceStatus> {
    if (!this.isSupported()) {
      this.currentStatus = 'error';
      return 'error';
    }

    const viVoices = await this.getVoices();
    if (viVoices.length === 0) {
      this.currentStatus = 'no-vietnamese-voice';
      return 'no-vietnamese-voice';
    }

    if (this.audioReady) {
      this.currentStatus = 'ready';
      return 'ready';
    }

    this.currentStatus = 'locked';
    return 'locked';
  }

  public async prime(): Promise<boolean> {
    if (!this.isSupported()) {
      this.currentStatus = 'error';
      return false;
    }

    const viVoices = await this.getVoices();
    if (viVoices.length === 0) {
      this.currentStatus = 'no-vietnamese-voice';
      return false;
    }

    return new Promise<boolean>((resolve) => {
      try {
        const synth = window.speechSynthesis;
        if (synth.paused) {
          synth.resume();
        }

        const UtteranceClass = (window as any).SpeechSynthesisUtterance || SpeechSynthesisUtterance;
        const u = new UtteranceClass(' ');
        u.volume = 0.01;
        u.rate = 1;

        let finished = false;
        const finish = (success: boolean) => {
          if (finished) return;
          finished = true;
          if (success) {
            this.audioReady = true;
            this.currentStatus = 'ready';
          } else {
            this.currentStatus = 'error';
          }
          resolve(success);
        };

        u.onend = () => finish(true);
        u.onerror = (e: any) => {
          if (e?.error === 'canceled' || e?.error === 'interrupted') {
            finish(true);
          } else {
            finish(false);
          }
        };

        synth.speak(u);
        setTimeout(() => finish(true), 250);
      } catch {
        this.currentStatus = 'error';
        resolve(false);
      }
    });
  }

  public async unlock(): Promise<boolean> {
    return this.prime();
  }

  public async getVoices(): Promise<VoiceInfo[]> {
    if (!this.isSupported()) {
      this.currentStatus = 'error';
      return [];
    }

    const synth = window.speechSynthesis;
    let rawVoices = synth.getVoices();

    if (rawVoices.length === 0) {
      rawVoices = await new Promise<SpeechSynthesisVoice[]>((resolve) => {
        let resolved = false;
        const onVoicesChanged = () => {
          if (resolved) return;
          resolved = true;
          synth.removeEventListener('voiceschanged', onVoicesChanged);
          resolve(synth.getVoices());
        };

        synth.addEventListener('voiceschanged', onVoicesChanged);

        setTimeout(() => {
          if (!resolved) {
            resolved = true;
            synth.removeEventListener('voiceschanged', onVoicesChanged);
            resolve(synth.getVoices());
          }
        }, 500);
      });
    }

    // STRICT VIETNAMESE ONLY: Never return foreign/English voices
    const vietnameseVoices = rawVoices.filter(isVietnameseVoice);
    if (rawVoices.length > 0 && vietnameseVoices.length === 0) {
      this.currentStatus = 'no-vietnamese-voice';
    } else if (vietnameseVoices.length > 0) {
      if (this.currentStatus === 'no-vietnamese-voice' || this.currentStatus === 'supported') {
        this.currentStatus = this.audioReady ? 'ready' : 'locked';
      }
    }

    return this.mapVoices(vietnameseVoices);
  }

  private mapVoices(voices: SpeechSynthesisVoice[]): VoiceInfo[] {
    return voices.map((v) => ({
      uri: v.voiceURI,
      name: v.name,
      lang: v.lang,
      isDefault: v.default,
    }));
  }

  public async speak(message: VoiceMessage): Promise<void> {
    if (!this.isSupported() || !message.text) return;

    const synth = window.speechSynthesis;

    if (synth.paused) {
      synth.resume();
    }

    const rawVoices = synth.getVoices();
    let selectedVoice: SpeechSynthesisVoice | undefined;

    if (message.voiceURI) {
      const match = rawVoices.find((v) => v.voiceURI === message.voiceURI);
      if (match && isVietnameseVoice(match)) {
        selectedVoice = match;
      }
    }

    if (!selectedVoice) {
      selectedVoice =
        rawVoices.find((v) => v.lang.toLowerCase().replace('_', '-') === 'vi-vn') ??
        rawVoices.find((v) => isVietnameseVoice(v));
    }

    // STRICT VIETNAMESE ONLY: Never fallback to English or default non-Vietnamese voices!
    if (!selectedVoice) {
      this.currentStatus = 'no-vietnamese-voice';
      throw new Error('BROWSER_VIETNAMESE_VOICE_UNAVAILABLE');
    }

    return new Promise((resolve, reject) => {
      const UtteranceClass = (window as any).SpeechSynthesisUtterance || SpeechSynthesisUtterance;
      const utterance = new UtteranceClass(message.text);
      utterance.voice = selectedVoice;
      utterance.lang = selectedVoice.lang || 'vi-VN';
      utterance.volume = message.volume ?? 1;
      utterance.rate = message.rate ?? 1.25;
      utterance.pitch = message.pitch ?? 1;

      let settled = false;

      utterance.onstart = () => {
        this.audioReady = true;
        this.currentStatus = 'ready';
        if (message.telemetry) {
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
        message.onStart?.(message.telemetry);
      };

      utterance.onend = () => {
        if (!settled) {
          settled = true;
          resolve();
        }
      };

      utterance.onerror = (event: SpeechSynthesisErrorEvent) => {
        if (!settled) {
          settled = true;
          if (event.error === 'canceled' || event.error === 'interrupted') {
            resolve();
          } else {
            this.currentStatus = 'error';
            reject(new Error(`BROWSER_SPEECH_ERROR: ${event.error || 'unknown'}`));
          }
        }
      };

      synth.speak(utterance);
    });
  }

  public cancel(): void {
    if (this.isSupported()) {
      try {
        window.speechSynthesis.cancel();
      } catch {}
    }
  }
}
