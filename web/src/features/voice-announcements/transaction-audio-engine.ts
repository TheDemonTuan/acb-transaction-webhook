import { isPublicViewerHost } from '../../app/runtime-mode';
import type { VoiceEngine, VoiceMessage } from './voice-engine';
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

interface ChimeNode { oscillator: OscillatorNode; gain: GainNode }

/** Schedule the same chime on realtime or offline Web Audio contexts. */
export function scheduleChime(context: BaseAudioContext, volume: number): { end: number; nodes: ChimeNode[] } {
  const level = Math.max(0, Math.min(1, volume));
  const nodes: ChimeNode[] = [];
  if (level === 0) return { end: 0, nodes };
  const start = context.currentTime;
  let end = start;
  for (const [frequency, offset, duration] of [[880, 0, 0.40], [1320, 0.30, 0.55]]) {
    const oscillator = context.createOscillator();
    const gain = context.createGain();
    oscillator.type = 'sine'; oscillator.frequency.setValueAtTime(frequency, start + offset);
    gain.gain.setValueAtTime(0.0001, start + offset);
    gain.gain.exponentialRampToValueAtTime(0.225 * level, start + offset + 0.01);
    gain.gain.exponentialRampToValueAtTime(0.060 * level, start + offset + duration * 0.5);
    gain.gain.exponentialRampToValueAtTime(0.0001, start + offset + duration);
    oscillator.connect(gain); gain.connect(context.destination);
    nodes.push({ oscillator, gain });
    oscillator.onended = () => { oscillator.disconnect(); gain.disconnect(); };
    oscillator.start(start + offset); oscillator.stop(start + offset + duration);
    end = Math.max(end, start + offset + duration);
  }
  return { end, nodes };
}

export class TransactionAudioEngine implements VoiceEngine {
  private audioContext: AudioContext | null = null;
  private readonly assets: AudioAssets;
  private readonly recent = new RecentPhraseRing();
  private readonly isPublic: boolean;
  private profile: Readonly<VoiceProfile> | null = null;
  private currentController: AbortController | null = null;
  private prefetched: PrefetchedPlan | null = null;
  private disposed = false;
  private chimeNodes: ChimeNode[] = [];

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
    return typeof window !== 'undefined' && Boolean(window.AudioContext || ('webkitAudioContext' in window && window.webkitAudioContext));
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
    return { isPublic: this.isPublic, context: this.audioContext, assets: this.assets, recent: this.recent,
      onStatus: this.options.onStatus };
  }
  private requireRunning(): AudioContext {
    if (!this.audioContext || this.audioContext.state !== 'running') throw new Error('AUDIO_CONTEXT_NOT_RUNNING');
    return this.audioContext;
  }
  public async unlock(): Promise<boolean> {
    if (this.disposed || !this.profile) return false;
    const context = this.context();
    if (!context) return false;
    try { if (context.state === 'suspended') await context.resume(); } catch { return false; }
    if (context.state !== 'running') return false;
    void this.assets.preload(this.profile, context).catch(() => {});
    return true;
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
    const context = this.audioContext;
    if (!context || context.state !== 'running') return 0;
    const chime = scheduleChime(context, volume);
    for (const node of chime.nodes) {
      const previous = node.oscillator.onended;
      node.oscillator.onended = (event) => {
        previous?.call(node.oscillator, event);
        this.chimeNodes = this.chimeNodes.filter((entry) => entry !== node);
      };
    }
    this.chimeNodes.push(...chime.nodes);
    return chime.end;
  }
  private clearPrefetch(): void {
    const prefetched = this.prefetched;
    this.prefetched = null;
    if (!prefetched) return;
    prefetched.detach(); prefetched.controller.abort();
    void prefetched.promise.then((plan) => plan.release()).catch(() => {});
  }
  // Decode may not be abortable. Settle the owner immediately, and release any late plan.
  private async prepare(message: VoiceMessage, profile: Readonly<VoiceProfile>, controller: AbortController,
      options: PreparationOptions): Promise<PreparedAnnouncement> {
    const cancelled = Promise.withResolvers<never>();
    const abort = () => cancelled.reject(new Error('VOICE_CANCELLED'));
    controller.signal.addEventListener('abort', abort, { once: true });
    const prepared = prepareAnnouncement(message, profile, controller.signal, options).then((plan) => {
      if (controller.signal.aborted) { plan.release(); throw new Error('VOICE_CANCELLED'); }
      return plan;
    });
    if (controller.signal.aborted) abort();
    try { return await Promise.race([prepared, cancelled.promise]); }
    finally { controller.signal.removeEventListener('abort', abort); }
  }
  public async prefetch(message: VoiceMessage, signal: AbortSignal): Promise<void> {
    let context: AudioContext | undefined;
    let controller: AbortController | undefined;
    let locked = false;
    const stateChanged = () => {
      if (context?.state === 'running') return;
      locked = true; controller?.abort();
    };
    try {
      if (signal.aborted) throw new Error('VOICE_CANCELLED');
      context = this.requireRunning();
      const profile = message.profile || this.profile;
      if (!profile) throw new Error('VOICE_PROFILE_UNAVAILABLE');
      if (this.prefetched?.message === message && this.prefetched.revision === profile.revision) {
        controller = this.prefetched.controller;
        await this.prefetched.promise; return;
      }
      this.clearPrefetch();
      controller = new AbortController();
      const ownedController = controller;
      const abort = () => ownedController.abort();
      signal.addEventListener('abort', abort, { once: true });
      context.addEventListener('statechange', stateChanged);
      const promise = this.prepare(message, profile, controller, this.preparationOptions());
      this.prefetched = { message, revision: profile.revision, controller, promise,
        detach: () => signal.removeEventListener('abort', abort) };
      await promise;
    } catch (error) {
      if (controller ? this.prefetched?.controller === controller : !signal.aborted) this.clearPrefetch();
      const failure = locked || (error instanceof Error && error.message === 'AUDIO_CONTEXT_NOT_RUNNING')
        ? Object.assign(new Error('AUDIO_CONTEXT_NOT_RUNNING'), { playbackStarted: false }) : error;
      this.options.onStatus?.('unavailable', failure instanceof Error ? failure.message : 'Giọng đọc không khả dụng');
      throw failure;
    } finally { context?.removeEventListener('statechange', stateChanged); }
  }
  public async speak(message: VoiceMessage): Promise<void> {
    if (this.disposed) throw new Error('VOICE_DISPOSED');
    if (this.currentController) throw new Error('VOICE_ENGINE_BUSY');
    let controller: AbortController | undefined;
    let context: AudioContext | undefined;
    let plan: PreparedAnnouncement | undefined;
    let locked = false;
    let playbackStarted = false;
    let restoreStart: (() => void) | undefined;
    const stateChanged = () => {
      if (context?.state === 'running') return;
      locked = true; controller?.abort();
      if (this.currentController === controller) this.stopChime();
    };
    try {
      context = this.requireRunning();
      const profile = message.profile || this.profile;
      if (!profile) throw new Error('VOICE_PROFILE_UNAVAILABLE');
      const prefetch = this.prefetched?.message === message && this.prefetched.revision === profile.revision ? this.prefetched : null;
      controller = prefetch?.controller || new AbortController();
      if (prefetch) { this.prefetched = null; prefetch.detach(); }
      this.currentController = controller;
      context.addEventListener('statechange', stateChanged);
      const options = this.preparationOptions();
      if (message.telemetry) message.telemetry.preparationStartedMonotonicAt = performance.now();
      const preparation = prefetch?.promise || this.prepare(message, profile, controller, options);
      // Own rejection before chime creation as well as while it is sounding.
      void preparation.catch(() => {});
      let chimeEnd = 0;
      let chimeFinished: Promise<void> | undefined;
      if (message.chime && profile.chimeEnabled) {
        chimeEnd = this.playChime(message.volume);
        if (chimeEnd > 0) {
          if (message.telemetry) {
            const now = performance.now();
            message.telemetry.chimeStartedMonotonicAt = now;
            if (message.telemetry.receivedMonotonicAt !== undefined) message.telemetry.sseToChimeMs = now - message.telemetry.receivedMonotonicAt;
          }
          const ended = Promise.withResolvers<void>();
          const last = this.chimeNodes.at(-1)!;
          const previous = last.oscillator.onended;
          last.oscillator.onended = (event) => {
            previous?.call(last.oscillator, event);
            if (message.telemetry) message.telemetry.chimeEndedMonotonicAt = performance.now();
            ended.resolve();
          };
          chimeFinished = ended.promise;
        }
      }
      plan = await preparation;
      this.requireRunning();
      if (message.telemetry) message.telemetry.audioReadyMonotonicAt = performance.now();
      if (controller.signal.aborted) throw new Error('VOICE_CANCELLED');
      const onTailStart = message.onTailStart;
      plan.message.onTailStart = (id) => { this.recent.started(id); onTailStart?.(id); };
      if (plan.kind === 'stream' && chimeFinished) {
        const cancelled = Promise.withResolvers<void>();
        const abort = () => cancelled.resolve();
        controller.signal.addEventListener('abort', abort, { once: true });
        if (controller.signal.aborted) abort();
        try { await Promise.race([chimeFinished, cancelled.promise]); }
        finally { controller.signal.removeEventListener('abort', abort); }
      }
      this.requireRunning();
      if (controller.signal.aborted) throw new Error('VOICE_CANCELLED');
      const onStart = plan.message.onStart;
      plan.message.onStart = (telemetry) => { playbackStarted = true; onStart?.(telemetry); };
      restoreStart = () => { if (plan) plan.message.onStart = onStart; };
      await playPrepared(plan, controller.signal, options, chimeEnd);
    } catch (error) {
      if (error instanceof Error && error.message === 'AUDIO_CONTEXT_NOT_RUNNING') locked = true;
      if (locked) this.clearPrefetch();
      const failure = locked ? Object.assign(new Error('AUDIO_CONTEXT_NOT_RUNNING'), { playbackStarted }) : error;
      if ((failure && typeof failure === 'object' && 'code' in failure && failure.code === 'VOICE_PROFILE_CHANGED') ||
          (failure instanceof Error && failure.message === 'AUDIO_PREPARATION_FAILED')) this.options.onProfileChanged?.();
      this.options.onStatus?.('unavailable', failure instanceof Error ? failure.message : 'Giọng đọc không khả dụng');
      throw failure;
    } finally {
      context?.removeEventListener('statechange', stateChanged);
      restoreStart?.(); controller?.abort(); plan?.release();
      if (this.currentController === controller) { this.stopChime(); this.currentController = null; }
    }
  }
  public cancel(): void {
    this.currentController?.abort(); this.currentController = null;
    this.assets.cancelPreload();
    this.clearPrefetch(); this.stopChime();
  }
  public dispose(): void {
    if (this.disposed) return;
    this.cancel(); this.disposed = true; this.assets.dispose();
    if (this.audioContext) { void this.audioContext.close().catch(() => {}); this.audioContext = null; }
  }
}
