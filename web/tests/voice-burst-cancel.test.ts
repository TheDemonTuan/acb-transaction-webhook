import { beforeEach, describe, expect, it } from 'vitest';
import { VoiceDedupe } from '../src/features/voice-announcements/voice-dedupe';
import { VoiceQueue } from '../src/features/voice-announcements/voice-queue';
import { buildSingleTransactionPhrase } from '../src/features/voice-announcements/voice-copy';
import type { VoiceEngine, VoiceInfo, VoiceMessage } from '../src/features/voice-announcements/voice-engine';

describe('Voice Queue Cancellation & Dedupe Lifecycle', () => {
  let dedupe: VoiceDedupe;

  beforeEach(() => {
    dedupe = new VoiceDedupe();
    dedupe.clear();
  });

  it('releases dedupe reservations when queued items are cancelled before playback completes', async () => {
    let cancelReject: ((err: Error) => void) | null = null;
    const engine: VoiceEngine = {
      isSupported: () => true,
      getVoices: async () => [],
      speak: (_msg) =>
        new Promise<void>((_, reject) => {
          cancelReject = reject;
        }),
      cancel: () => {
        if (cancelReject) {
          cancelReject(new Error('VOICE_CANCELLED'));
          cancelReject = null;
        }
      },
    };

    const queue = new VoiceQueue(engine);
    const opts1 = { eventId: 'ev_1', transactionId: 'tx_1' };
    const opts2 = { eventId: 'ev_2', transactionId: 'tx_2' };

    expect(dedupe.reserve(opts1)).toBe(true);
    expect(dedupe.reserve(opts2)).toBe(true);
    expect(dedupe.isReserved(opts1)).toBe(true);
    expect(dedupe.isReserved(opts2)).toBe(true);

    queue.enqueue({
      text: buildSingleTransactionPhrase('100000', 'Test 1'),
      transactionId: opts1.transactionId,
      onSuccess: () => dedupe.commit(opts1),
      onError: () => dedupe.release(opts1),
    });
    queue.enqueue({
      text: buildSingleTransactionPhrase('200000', 'Test 2'),
      transactionId: opts2.transactionId,
      onSuccess: () => dedupe.commit(opts2),
      onError: () => dedupe.release(opts2),
    });

    // Both items are active/queued and reserved
    expect(dedupe.isReserved(opts1)).toBe(true);
    expect(dedupe.isReserved(opts2)).toBe(true);

    // Cancel queue directly (as cancelVoice / settings disable does)
    queue.cancel();

    // Allow microtask to settle for the currently speaking utterance
    await Promise.resolve();

    // Both items must be released and free to be re-reserved
    expect(dedupe.isReserved(opts1)).toBe(false);
    expect(dedupe.isReserved(opts2)).toBe(false);
    expect(dedupe.has(opts1)).toBe(false);
    expect(dedupe.has(opts2)).toBe(false);

    // Can reserve again cleanly
    expect(dedupe.reserve(opts1)).toBe(true);
    expect(dedupe.reserve(opts2)).toBe(true);
  });

  it('commits dedupe reservation when playback succeeds', async () => {
    const engine: VoiceEngine = {
      isSupported: () => true,
      getVoices: async () => [],
      speak: async () => {},
      cancel: () => {},
    };

    const queue = new VoiceQueue(engine);
    const opts = { eventId: 'ev_success', transactionId: 'tx_success' };

    expect(dedupe.reserve(opts)).toBe(true);

    queue.enqueue({
      text: buildSingleTransactionPhrase('500000', 'Test success'),
      transactionId: opts.transactionId,
      onSuccess: () => dedupe.commit(opts),
      onError: () => dedupe.release(opts),
    });

    await new Promise((r) => setTimeout(r, 20));

    expect(dedupe.isReserved(opts)).toBe(false);
    expect(dedupe.has(opts)).toBe(true);
  });

  it('directly enqueues single transaction phrase without burst buffer aggregation', () => {
    const spoken: string[] = [];
    const engine: VoiceEngine = {
      isSupported: () => true,
      getVoices: async () => [],
      speak: async (msg) => {
        spoken.push(msg.text);
      },
      cancel: () => {},
    };

    const queue = new VoiceQueue(engine);
    const opts = { eventId: 'ev_direct', transactionId: 'tx_direct' };
    expect(dedupe.reserve(opts)).toBe(true);

    const text = buildSingleTransactionPhrase('500000', 'Test direct', {
      includeDescription: true,
    });
    queue.enqueue({
      text,
      transactionId: opts.transactionId,
      includeDescription: true,
      onSuccess: () => dedupe.commit(opts),
      onError: () => dedupe.release(opts),
    });

    // Immediate enqueue transitions state to speaking without 750ms burst delay
    expect(queue.getState()).toBe('speaking');
  });
});
