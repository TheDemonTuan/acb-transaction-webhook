import { describe, expect, it } from 'bun:test';
import type { VoiceEngine, VoiceInfo, VoiceMessage } from '../src/features/voice-announcements/voice-engine';
import { VoiceQueue } from '../src/features/voice-announcements/voice-queue';

class FakeVoiceEngine implements VoiceEngine {
  public spoken: string[] = [];
  public prefetched: string[] = [];
  public cancelled = false;
  public disposed = false;
  public blocker: Promise<void> | null = null;

  isSupported(): boolean {
    return true;
  }
  async getVoices(): Promise<VoiceInfo[]> {
    return [];
  }
  async speak(message: VoiceMessage): Promise<void> {
    if (this.blocker) {
      await this.blocker;
    }
    this.spoken.push(message.text);
  }
  async prefetch(message: VoiceMessage, signal: AbortSignal): Promise<void> {
    if (signal.aborted) throw new Error('VOICE_CANCELLED');
    this.prefetched.push(message.text);
  }
  cancel(): void {
    this.cancelled = true;
  }
  dispose(): void {
    this.disposed = true;
  }
}

describe('VoiceQueue', () => {
  it('processes messages sequentially and triggers prefetch', async () => {
    const fake = new FakeVoiceEngine();
    const queue = new VoiceQueue(fake, undefined, { prefetch: 1 });

    const p1 = queue.speak({ text: 'Câu thứ nhất' });
    const p2 = queue.speak({ text: 'Câu thứ hai' });

    expect(fake.prefetched).toContain('Câu thứ hai');

    await Promise.all([p1, p2]);

    expect(fake.spoken).toEqual(['Câu thứ nhất', 'Câu thứ hai']);
    expect(queue.getState()).toBe('idle');
  });

  it('bounds queue capacity to maxPending (20) and drops overflow', () => {
    const fake = new FakeVoiceEngine();
    const { promise: blocker, resolve: unblock } = Promise.withResolvers<void>();
    fake.blocker = blocker;
    const queue = new VoiceQueue(fake, undefined, { maxPending: 20 });

    // First item starts speaking and blocks on fake.blocker
    queue.enqueue({ text: 'Current speaking' });
    expect(queue.getState()).toBe('speaking');

    // Enqueue 20 items (maxPending = 20)
    for (let i = 1; i <= 20; i++) {
      const res = queue.enqueue({ text: `Pending ${i}` });
      expect(res).toBe('queued');
    }
    expect(queue.getPendingCount()).toBe(20);

    // 21st item overflows
    let errorCalled = false;
    const overflowRes = queue.enqueue({
      text: 'Overflow item',
      onError: (err) => {
        if (err instanceof Error && err.message === 'QUEUE_OVERFLOW') {
          errorCalled = true;
        }
      },
    });
    expect(overflowRes).toBe('overflow');
    expect(errorCalled).toBe(true);
    expect(queue.getPendingCount()).toBe(20);

    unblock();
    queue.cancel();
  });

  it('drops items older than maxAgeMs (30s) as expired', () => {
    const fake = new FakeVoiceEngine();
    const queue = new VoiceQueue(fake, undefined, { maxAgeMs: 30000 });

    let expiredError = false;
    const oldTimestamp = (typeof performance !== 'undefined' ? performance.now() : Date.now()) - 35000;
    const res = queue.enqueue({
      text: 'Expired message',
      queuedMonotonicAt: oldTimestamp,
      onError: (err) => {
        if (err instanceof Error && err.message === 'EXPIRED_AGE') {
          expiredError = true;
        }
      },
    });

    expect(res).toBe('expired');
    expect(expiredError).toBe(true);
    expect(queue.getPendingCount()).toBe(0);
  });

  it('burst 4 accepted items retains chosen recipe and pending <= 20 without implicit shortening', () => {
    const fake = new FakeVoiceEngine();
    const { promise: blocker, resolve: unblock } = Promise.withResolvers<void>();
    fake.blocker = blocker;
    const queue = new VoiceQueue(fake, undefined, { maxPending: 20 });

    queue.enqueue({ text: 'Current' });
    const msg1: VoiceMessage = { text: 'Item 1', phraseId: 'recipe-1' };
    const msg2: VoiceMessage = { text: 'Item 2', phraseId: 'recipe-2' };
    const msg3: VoiceMessage = { text: 'Item 3', phraseId: 'recipe-3' };
    const msg4: VoiceMessage = { text: 'Item 4', phraseId: 'recipe-4' };

    expect(queue.enqueue(msg1)).toBe('queued');
    expect(queue.enqueue(msg2)).toBe('queued');
    expect(queue.enqueue(msg3)).toBe('queued');
    expect(queue.enqueue(msg4)).toBe('queued');

    expect(queue.getPendingCount()).toBe(4);
    expect(msg1.phraseId).toBe('recipe-1');
    expect(msg2.phraseId).toBe('recipe-2');
    expect(msg3.phraseId).toBe('recipe-3');
    expect(msg4.phraseId).toBe('recipe-4');

    unblock();
    queue.cancel();
  });

  it('cancels queue and engine cleanly', () => {
    const fake = new FakeVoiceEngine();
    const queue = new VoiceQueue(fake);

    queue.enqueue({ text: 'Câu 1' });
    queue.cancel();

    expect(fake.cancelled).toBe(true);
    expect(queue.getState()).toBe('idle');
  });

  it('prefetches one pending owner once despite burst enqueue and aborts it on clear', () => {
    const fake = new FakeVoiceEngine();
    const current = Promise.withResolvers<void>(); fake.blocker = current.promise;
    const signals: AbortSignal[] = [];
    fake.prefetch = async (message, signal) => { fake.prefetched.push(message.text); signals.push(signal); };
    const queue = new VoiceQueue(fake);
    queue.enqueue({ text: 'current' });
    for (let index = 0; index < 20; index++) queue.enqueue({ text: `pending-${index}` });
    expect(fake.prefetched).toEqual(['pending-0']);
    expect(signals).toHaveLength(1);
    queue.clear();
    expect(signals[0].aborted).toBe(true);
    expect(queue.getPendingCount()).toBe(0);
    current.resolve(); queue.cancel();
  });

  it('dispose clears pending owners and disposes the engine', () => {
    const fake = new FakeVoiceEngine();
    const current = Promise.withResolvers<void>(); fake.blocker = current.promise;
    const queue = new VoiceQueue(fake);
    queue.enqueue({ text: 'current' }); queue.enqueue({ text: 'next' });
    queue.dispose();
    expect(fake.disposed).toBe(true); expect(fake.cancelled).toBe(true);
    expect(queue.getPendingCount()).toBe(0);
    current.resolve();
  });
});
