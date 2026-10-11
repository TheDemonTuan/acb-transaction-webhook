import type { VoiceEngine, VoiceInfo, VoiceMessage } from './voice-engine';

export type BrowserVoiceStatus = 'supported' | 'no-vietnamese-voice' | 'locked' | 'ready' | 'error';
export function isVietnameseVoice(voice: { lang?: string }): boolean {
  const lang = voice.lang?.toLowerCase().replace('_', '-');
  return lang === 'vi' || Boolean(lang?.startsWith('vi-'));
}

export class BrowserSpeechEngine implements VoiceEngine {
  private currentStatus: BrowserVoiceStatus = 'supported';
  private audioReady = false;
  private disposed = false;
  private cancelCurrent: (() => void) | null = null;
  private voiceWaits = new Set<() => void>();

  public isSupported(): boolean {
    return !this.disposed && typeof window !== 'undefined' && 'speechSynthesis' in window && 'SpeechSynthesisUtterance' in window;
  }
  public getStatus(): BrowserVoiceStatus { return this.isSupported() ? this.currentStatus : 'error'; }
  public async checkStatus(): Promise<BrowserVoiceStatus> {
    if (!this.isSupported()) return 'error';
    const voices = await this.getVoices();
    this.currentStatus = voices.length === 0 ? 'no-vietnamese-voice' : this.audioReady ? 'ready' : 'locked';
    return this.currentStatus;
  }
  public async getVoices(): Promise<VoiceInfo[]> {
    if (!this.isSupported()) return [];
    const synth = window.speechSynthesis;
    let voices = synth.getVoices();
    if (!voices.length) {
      const { promise, resolve } = Promise.withResolvers<SpeechSynthesisVoice[]>();
      let settled = false;
      const finish = () => {
        if (settled) return;
        settled = true; clearTimeout(timer); synth.removeEventListener('voiceschanged', finish);
        this.voiceWaits.delete(finish); resolve(synth.getVoices());
      };
      const timer = setTimeout(finish, 500);
      this.voiceWaits.add(finish); synth.addEventListener('voiceschanged', finish);
      voices = await promise;
    }
    return voices.filter(isVietnameseVoice).map((voice) => ({ uri: voice.voiceURI, name: voice.name, lang: voice.lang, isDefault: voice.default }));
  }
  public resolveVoice(voiceURI?: string): SpeechSynthesisVoice | undefined {
    if (!this.isSupported() || !voiceURI) return;
    return window.speechSynthesis.getVoices().find((voice) => voice.voiceURI === voiceURI && isVietnameseVoice(voice));
  }
  public async prefetch(message: VoiceMessage, signal: AbortSignal): Promise<void> {
    if (signal.aborted) throw new Error('VOICE_CANCELLED');
    if (message.profile?.providerMode !== 'BROWSER_ONLY') throw new Error('BROWSER_MODE_NOT_SELECTED');
    if (!this.resolveVoice(message.voiceURI)) {
      this.currentStatus = 'no-vietnamese-voice'; throw new Error('BROWSER_VIETNAMESE_VOICE_UNAVAILABLE');
    }
  }
  public async prime(): Promise<boolean> {
    if (!this.isSupported() || !(await this.getVoices()).length) return false;
    const synth = window.speechSynthesis;
    const utterance = new window.SpeechSynthesisUtterance(' ');
    utterance.volume = 0.01;
    const { promise, resolve } = Promise.withResolvers<boolean>();
    let settled = false;
    const finish = (success: boolean) => {
      if (settled) return;
      settled = true; clearTimeout(timer); utterance.onend = null; utterance.onerror = null;
      this.cancelCurrent = null;
      this.audioReady = success; this.currentStatus = success ? 'ready' : 'error'; resolve(success);
    };
    const timer = setTimeout(() => { finish(false); synth.cancel(); }, 1500);
    this.cancelCurrent = () => finish(false);
    utterance.onend = () => finish(true); utterance.onerror = () => finish(false);
    try { if (synth.paused) synth.resume(); synth.speak(utterance); } catch { finish(false); }
    return promise;
  }
  public async unlock(): Promise<boolean> { return this.prime(); }
  public async speak(message: VoiceMessage): Promise<void> {
    await this.prefetch(message, new AbortController().signal);
    const selected = this.resolveVoice(message.voiceURI);
    if (!selected) throw new Error('BROWSER_VIETNAMESE_VOICE_UNAVAILABLE');
    if (!message.text) throw new Error('BROWSER_SPEECH_EMPTY');
    const synth = window.speechSynthesis;
    const utterance = new window.SpeechSynthesisUtterance(message.text);
    utterance.voice = selected; utterance.lang = selected.lang;
    utterance.volume = Math.max(0, Math.min(1, message.volume ?? 1));
    utterance.rate = message.profile!.rate;
    utterance.pitch = 1 + message.profile!.pitchHz / 50;
    const { promise, resolve, reject } = Promise.withResolvers<void>();
    let settled = false, started = false, tailStarted = false;
    const finish = (error?: Error) => {
      if (settled) return;
      settled = true; clearTimeout(timer);
      utterance.onstart = null; utterance.onend = null; utterance.onerror = null; utterance.onboundary = null;
      this.cancelCurrent = null;
      if (error) reject(Object.assign(error, { playbackStarted: started })); else resolve();
    };
    const timer = setTimeout(() => { finish(new Error('AUDIO_PLAYBACK_TIMEOUT')); synth.cancel(); }, 120000);
    this.cancelCurrent = () => finish(new Error('VOICE_CANCELLED'));
    utterance.onstart = () => {
      if (started || settled) return;
      started = true; this.audioReady = true; this.currentStatus = 'ready';
      const now = performance.now();
      if (message.telemetry) {
        message.telemetry.speechStartedAt = Date.now(); message.telemetry.speechStartedMonotonicAt = now;
        if (message.telemetry.receivedMonotonicAt !== undefined) message.telemetry.sseToSpeakMs = now - message.telemetry.receivedMonotonicAt;
        if (message.queuedMonotonicAt !== undefined) message.telemetry.queueToSpeakMs = now - message.queuedMonotonicAt;
      }
      message.onStart?.(message.telemetry);
      if (message.recipe?.styleId === 'classic') { tailStarted = true; message.onTailStart?.(message.recipe.id); }
    };
    utterance.onboundary = (event) => {
      const tailIndex = message.recipe?.tail ? message.text.indexOf(message.recipe.tail) : -1;
      if (!tailStarted && message.recipe && tailIndex >= 0 && event.charIndex >= tailIndex) {
        tailStarted = true; message.onTailStart?.(message.recipe.id);
      }
    };
    utterance.onend = () => {
      if (!tailStarted && message.recipe) message.onTailStart?.(message.recipe.id);
      finish();
    };
    utterance.onerror = (event) => { this.currentStatus = 'error'; finish(new Error(`BROWSER_SPEECH_ERROR: ${event.error}`)); };
    try { if (synth.paused) synth.resume(); synth.speak(utterance); } catch (error) { finish(error instanceof Error ? error : new Error(String(error))); }
    return promise;
  }
  public cancel(): void {
    this.cancelCurrent?.();
    if (this.isSupported()) window.speechSynthesis.cancel();
    for (const finish of this.voiceWaits) finish();
  }
  public dispose(): void {
    if (this.disposed) return;
    this.cancel(); this.disposed = true;
  }
}
