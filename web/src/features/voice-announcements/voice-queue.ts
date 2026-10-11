import type { VoiceEngine, VoiceMessage } from './voice-engine';
import type { VoiceProfile } from './voice-profile';
export type VoiceQueueState = 'idle' | 'speaking';
export interface VoiceQueueOptions { maxPending?: number; maxAgeMs?: number; prefetch?: number; getProfile?: () => Readonly<VoiceProfile> | undefined }
export type EnqueueResult = 'queued' | 'expired' | 'overflow';

export class VoiceQueue {
  private queue: VoiceMessage[] = [];
  private state: VoiceQueueState = 'idle';
  private operationId = 0;
  private currentItem: VoiceMessage | null = null;
  private prefetchController: AbortController | null = null;
  private prefetchedItem: VoiceMessage | null = null;
  private readonly maxPending: number;
  private readonly maxAgeMs: number;
  private readonly prefetchEnabled: boolean;
  private readonly getProfile?: () => Readonly<VoiceProfile> | undefined;
  constructor(private readonly engine: VoiceEngine, private readonly onStateChange?: (state: VoiceQueueState) => void,
      options?: VoiceQueueOptions) {
    this.maxPending = Math.max(0, options?.maxPending ?? 20);
    this.maxAgeMs = Math.max(0, options?.maxAgeMs ?? 30000);
    this.prefetchEnabled = (options?.prefetch ?? 1) > 0;
    this.getProfile = options?.getProfile;
  }
  public getState(): VoiceQueueState { return this.state; }
  public getPendingCount(): number { return this.queue.length; }
  public getCancellationGeneration(): number { return this.operationId; }
  private setState(state: VoiceQueueState): void {
    if (this.state === state) return;
    this.state = state; this.onStateChange?.(state);
  }
  public enqueue(message: VoiceMessage): EnqueueResult {
    const now = performance.now();
    message.queuedMonotonicAt ??= now;
    if (now - message.queuedMonotonicAt > this.maxAgeMs) {
      message.onError?.(new Error('EXPIRED_AGE')); return 'expired';
    }
    if (this.currentItem && this.queue.length >= this.maxPending) {
      message.onError?.(new Error('QUEUE_OVERFLOW')); return 'overflow';
    }
    this.queue.push(message);
    if (!this.currentItem) void this.processNext();
    else this.triggerPrefetch();
    return 'queued';
  }
  public async speak(message: VoiceMessage): Promise<void> {
    const { promise, resolve, reject } = Promise.withResolvers<void>();
    this.enqueue({ ...message, onSuccess: () => { message.onSuccess?.(); resolve(); },
      onError: (error) => { message.onError?.(error); reject(error); } });
    return promise;
  }
  private triggerPrefetch(): void {
    if (!this.prefetchEnabled || !this.currentItem || this.queue.length === 0) return;
    const next = this.queue[0];
    if (next === this.prefetchedItem) return;
    this.prefetchController?.abort();
    this.prefetchedItem = next; this.prefetchController = new AbortController();
    next.profile ??= this.getProfile?.();
    void this.engine.prefetch(next, this.prefetchController.signal).catch(() => {});
  }
  private async processNext(): Promise<void> {
    const operation = this.operationId;
    let message: VoiceMessage | undefined;
    while ((message = this.queue.shift())) {
      if (performance.now() - (message.queuedMonotonicAt ?? performance.now()) <= this.maxAgeMs) break;
      if (message === this.prefetchedItem) {
        this.prefetchController?.abort(); this.prefetchController = null; this.prefetchedItem = null;
      }
      message.onError?.(new Error('EXPIRED_AGE'));
    }
    if (!message) { this.currentItem = null; this.setState('idle'); return; }
    // Transfer next's prepared owner to speak before prefetched ownership changes.
    if (message === this.prefetchedItem) { this.prefetchedItem = null; this.prefetchController = null; }
    this.currentItem = message; this.setState('speaking');
    try {
      message.profile ??= this.getProfile?.();
      const speaking = this.engine.speak(message);
      this.triggerPrefetch();
      await speaking;
      if (operation !== this.operationId) message.onError?.(new Error('VOICE_CANCELLED'));
      else message.onSuccess?.();
    } catch (error) { message.onError?.(error); }
    finally { if (operation === this.operationId) this.currentItem = null; }
    if (operation === this.operationId) void this.processNext();
  }
  public refreshPendingProfile(): void {
    this.prefetchController?.abort(); this.prefetchController = null; this.prefetchedItem = null;
    for (const message of this.queue) { message.profile = undefined; message.recipe = undefined; message.phraseId = undefined; }
    this.triggerPrefetch();
  }
  public cancel(): void {
    this.operationId++;
    this.clear(); this.currentItem = null; this.engine.cancel(); this.setState('idle');
  }
  public clear(): void {
    this.prefetchController?.abort(); this.prefetchController = null; this.prefetchedItem = null;
    const remaining = this.queue; this.queue = [];
    for (const message of remaining) message.onError?.(new Error('VOICE_CANCELLED'));
  }
  public dispose(): void {
    this.cancel(); void this.engine.dispose();
  }
}
