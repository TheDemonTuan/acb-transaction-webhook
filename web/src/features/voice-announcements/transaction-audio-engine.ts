import { isPublicViewerHost } from '../../app/runtime-mode';
import { BrowserSpeechEngine } from './browser-speech-engine';
import type { VoiceEngine, VoiceInfo, VoiceMessage } from './voice-engine';
import type { VoiceProfile } from './voice-profile';
import { AudioAssets } from './audio-assets';
import { RecentPhraseRing } from './voice-copy';
import { prepareAnnouncement, playPrepared, type PreparedAnnouncement, type PreparationOptions } from './announcement-plan';

export interface TransactionAudioEngineOptions {
  isPublic?: boolean;
  onStatus?: PreparationOptions['onStatus'];
  onProfileChanged?: () => void;
}
interface PrefetchedPlan {
  message: VoiceMessage;
  revision: number;
  controller: AbortController;
  promise: Promise<PreparedAnnouncement>;
  detach: () => void;
}

export class TransactionAudioEngine implements VoiceEngine {
  private audioContext: AudioContext | null = null;
  private readonly browser = new BrowserSpeechEngine();
  private readonly assets: AudioAssets;
  private readonly recent = new RecentPhraseRing();
  private readonly isPublic: boolean;
  private profile: Readonly<VoiceProfile> | null = null;
  private currentController: AbortController | null = null;
  private prefetched: PrefetchedPlan | null = null;
  private disposed = false;
  private chimeNodes: { oscillator: OscillatorNode; gain: GainNode }[] = [];

  constructor(private readonly options: TransactionAudioEngineOptions = {}) {
    this.isPublic = options.isPublic ?? (typeof window !== 'undefined' && isPublicViewerHost());
    this.assets = new AudioAssets(this.isPublic);
  }
  public setProfile(profile: Readonly<VoiceProfile>): void {
    if (this.profile?.revision === profile.revision) return;
    this.profile = Object.freeze({ ...profile, amountBands: profile.amountBands.map((band) => ({ ...band })) });
    this.clearPrefetch();
    this.assets.changeProfile(profile.synthesisKey);
  }
  public isSupported(): boolean {
    return typeof window !== 'undefined' && Boolean(window.AudioContext || ('webkitAudioContext' in window && window.webkitAudioContext) || window.Audio || this.browser.isSupported());
  }
  private context(): AudioContext | null {
    if (this.disposed) throw new Error('VOICE_DISPOSED');
    if (!this.audioContext && typeof window !== 'undefined') {
      const legacy = window as Window & { webkitAudioContext?: typeof AudioContext };
      const AudioContextClass = window.AudioContext || legacy.webkitAudioContext;
      if (AudioContextClass) this.audioContext = new AudioContextClass();
    }
    return this.audioContext;
  }
  private preparationOptions(): PreparationOptions {
    return { isPublic: this.isPublic, context: this.context(), assets: this.assets, recent: this.recent,
      browser: this.browser, onStatus: this.options.onStatus };
  }
  public async unlock(): Promise<boolean> {
    if (this.disposed || !this.profile) return false;
    if (this.profile.providerMode === 'BROWSER_ONLY') return this.browser.unlock();
    const context = this.context();
    if (context) {
      try { if (context.state === 'suspended') await context.resume(); } catch { return false; }
      if (context.state !== 'running') return false;
      void this.assets.preload(this.profile, context).catch(() => {});
      return true;
    }
    return typeof window !== 'undefined' && Boolean(window.Audio);
  }
  public async getVoices(): Promise<VoiceInfo[]> {
    // These are device-only Vietnamese choices; server voices come from the profile.
    return this.browser.getVoices();
  }
  private stopChime(): void {
    for (const { oscillator, gain } of this.chimeNodes) {
      oscillator.onended = null;
      try { oscillator.stop(); } catch {}
      oscillator.disconnect(); gain.disconnect();
    }
    this.chimeNodes = [];
  }
  public playChime(volume = 1): number {
    const context = this.context();
    if (!context || context.state !== 'running' || volume <= 0) return 0;
    const start = context.currentTime;
    for (const [frequency, offset, duration] of [[880, 0, 0.12], [1320, 0.12, 0.13]]) {
      const oscillator = context.createOscillator();
      const gain = context.createGain();
      oscillator.type = 'sine'; oscillator.frequency.setValueAtTime(frequency, start + offset);
      gain.gain.setValueAtTime(0.0001, start + offset);
      gain.gain.exponentialRampToValueAtTime(0.15 * Math.min(1, volume), start + offset + 0.02);
      gain.gain.exponentialRampToValueAtTime(0.0001, start + offset + duration);
      oscillator.connect(gain); gain.connect(context.destination);
      const node = { oscillator, gain };
      this.chimeNodes.push(node);
      oscillator.onended = () => {
        oscillator.disconnect(); gain.disconnect();
        this.chimeNodes = this.chimeNodes.filter((entry) => entry !== node);
      };
      oscillator.start(start + offset); oscillator.stop(start + offset + duration);
    }
    return start + 0.25;
  }
  private clearPrefetch(): void {
    const prefetched = this.prefetched;
    this.prefetched = null;
    if (!prefetched) return;
    prefetched.detach(); prefetched.controller.abort();
    void prefetched.promise.then((plan) => plan.release()).catch(() => {});
  }
  public async prefetch(message: VoiceMessage, signal: AbortSignal): Promise<void> {
    if (signal.aborted) throw new Error('VOICE_CANCELLED');
    const profile = message.profile || this.profile;
    if (!profile) throw new Error('VOICE_PROFILE_UNAVAILABLE');
    if (this.prefetched?.message === message && this.prefetched.revision === profile.revision) {
      await this.prefetched.promise; return;
    }
    this.clearPrefetch();
    const controller = new AbortController();
    const abort = () => controller.abort();
    signal.addEventListener('abort', abort, { once: true });
    const options = this.preparationOptions();
    const promise = prepareAnnouncement(message, profile, controller.signal, options);
    this.prefetched = { message, revision: profile.revision, controller, promise, detach: () => signal.removeEventListener('abort', abort) };
    await promise;
  }
  public async speak(message: VoiceMessage): Promise<void> {
    if (this.disposed) throw new Error('VOICE_DISPOSED');
    if (this.currentController) throw new Error('VOICE_ENGINE_BUSY');
    const profile = message.profile || this.profile;
    if (!profile) throw new Error('VOICE_PROFILE_UNAVAILABLE');
    const prefetch = this.prefetched?.message === message && this.prefetched.revision === profile.revision ? this.prefetched : null;
    let controller: AbortController;
    let preparation: Promise<PreparedAnnouncement>;
    const options = this.preparationOptions();
    if (prefetch) {
      this.prefetched = null; prefetch.detach();
      controller = prefetch.controller; preparation = prefetch.promise;
    } else {
      controller = new AbortController();
      preparation = prepareAnnouncement(message, profile, controller.signal, options);
    }
    this.currentController = controller;
    let plan: PreparedAnnouncement | undefined;
    try {
      // Preparation begins before the chime, so the first await never delays it.
      const chimeEnd = message.chime && profile.chimeEnabled ? this.playChime(message.volume) : 0;
      plan = await preparation;
      if (controller.signal.aborted) throw new Error('VOICE_CANCELLED');
      const onTailStart = message.onTailStart;
      plan.message.onTailStart = (id) => { this.recent.started(id); onTailStart?.(id); };
      if (plan.kind !== 'buffers' && chimeEnd && options.context && options.context.currentTime < chimeEnd) {
        // Native/browser cannot schedule against AudioContext. Wait for the actual
        // oscillator completion, not an arbitrary sleep between utterances.
        const { promise, resolve } = Promise.withResolvers<void>();
        const last = this.chimeNodes.at(-1);
        if (last) {
          const previous = last.oscillator.onended;
          last.oscillator.onended = (event) => { previous?.call(last.oscillator, event); resolve(); };
          const abort = () => resolve();
          controller.signal.addEventListener('abort', abort, { once: true });
          await promise;
          controller.signal.removeEventListener('abort', abort);
        }
      }
      await playPrepared(plan, controller.signal, options, chimeEnd);
    } catch (error) {
      if ((error && typeof error === 'object' && 'code' in error && error.code === 'VOICE_PROFILE_CHANGED') ||
          (error instanceof Error && error.message === 'AUDIO_PREPARATION_FAILED')) this.options.onProfileChanged?.();
      this.options.onStatus?.('unavailable', error instanceof Error ? error.message : 'Giọng đọc không khả dụng');
      throw error;
    } finally {
      plan?.release();
      if (this.currentController === controller) this.currentController = null;
    }
  }
  public cancel(): void {
    this.currentController?.abort(); this.currentController = null;
    this.assets.cancelPreload();
    this.clearPrefetch(); this.stopChime(); this.browser.cancel();
  }
  public dispose(): void {
    if (this.disposed) return;
    this.cancel(); this.disposed = true; this.assets.dispose(); this.browser.dispose();
    if (this.audioContext) { void this.audioContext.close().catch(() => {}); this.audioContext = null; }
  }
}
