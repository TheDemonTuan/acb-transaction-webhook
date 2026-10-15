import { beforeEach, describe, expect, it } from 'bun:test';
import { VoiceDedupe } from '../src/features/voice-announcements/voice-dedupe';

describe('VoiceDedupe', () => {
  let dedupe: VoiceDedupe;

  beforeEach(() => {
    dedupe = new VoiceDedupe();
    dedupe.clear();
  });

  it('marks and detects duplicate by eventId', () => {
    expect(dedupe.has({ eventId: 'ep1:100' })).toBe(false);
    dedupe.mark({ eventId: 'ep1:100' });
    expect(dedupe.has({ eventId: 'ep1:100' })).toBe(true);
  });

  it('marks and detects duplicate by transactionId', () => {
    expect(dedupe.has({ transactionId: 'tx_123' })).toBe(false);
    dedupe.mark({ transactionId: 'tx_123' });
    expect(dedupe.has({ transactionId: 'tx_123' })).toBe(true);
    expect(dedupe.has({ eventId: 'ep1:different', transactionId: 'tx_123' })).toBe(true);
  });

  it('handles two-phase reservation: reserve, commit, release', () => {
    const opts = { transactionId: 'tx_phase_1' };
    expect(dedupe.has(opts)).toBe(false);
    expect(dedupe.isReserved(opts)).toBe(false);

    // Reserve successfully
    expect(dedupe.reserve(opts)).toBe(true);
    expect(dedupe.isReserved(opts)).toBe(true);
    // Second reserve fails while reserved
    expect(dedupe.reserve(opts)).toBe(false);

    // Release reservation (e.g. on temporary error)
    dedupe.release(opts);
    expect(dedupe.isReserved(opts)).toBe(false);
    expect(dedupe.has(opts)).toBe(false);

    // Re-reserve and commit on playback success
    expect(dedupe.reserve(opts)).toBe(true);
    dedupe.commit(opts);
    expect(dedupe.isReserved(opts)).toBe(false);
    expect(dedupe.has(opts)).toBe(true);

    // Cannot reserve after commit
    expect(dedupe.reserve(opts)).toBe(false);
  });

  it('marks skipped events as terminal', () => {
    const opts = { transactionId: 'tx_skipped_1' };
    dedupe.markSkipped(opts, 'EXPIRED_FRESHNESS');
    expect(dedupe.has(opts)).toBe(true);
    expect(dedupe.reserve(opts)).toBe(false);
  });

  it('detects stale event older than max age', () => {
    const now = Date.now();
    const freshIso = new Date(now - 10_000).toISOString(); // 10s ago
    const staleIso = new Date(now - 200_000).toISOString(); // 200s ago

    expect(dedupe.isFreshEnough(freshIso, 120_000, now)).toBe(true);
    expect(dedupe.isFreshEnough(staleIso, 120_000, now)).toBe(false);
  });

  it('allows future clock skew up to 30s and rejects extreme future', () => {
    const now = Date.now();
    const slightFutureIso = new Date(now + 15_000).toISOString(); // 15s into future (clock skew)
    const exactFuture30sIso = new Date(now + 30_000).toISOString(); // exactly 30s
    const extremeFutureIso = new Date(now + 60_000).toISOString(); // 60s into future

    expect(dedupe.isFreshEnough(slightFutureIso, 120_000, now)).toBe(true);
    expect(dedupe.isFreshEnough(exactFuture30sIso, 120_000, now)).toBe(true);
    expect(dedupe.isFreshEnough(extremeFutureIso, 120_000, now)).toBe(false);
  });
});
