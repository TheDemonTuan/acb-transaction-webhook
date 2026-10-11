import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TransactionAudioEngine } from '../src/features/voice-announcements/transaction-audio-engine';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import { voiceRoute } from '../src/features/voice-announcements/announcement-plan';

class NativeAudio extends EventTarget {
  static owners: NativeAudio[] = [];
  static failPrepare = false;
  src = ''; volume = 1; playbackRate = 1; preload = '';
  stopped = false; play = vi.fn(async () => {
    this.dispatchEvent(new Event('playing')); this.dispatchEvent(new Event('playing'));
    queueMicrotask(() => this.dispatchEvent(new Event('ended')));
  });
  constructor() { super(); NativeAudio.owners.push(this); }
  pause() { this.stopped = true; }
  removeAttribute() { this.src = ''; }
  load() {
    if (this.src) queueMicrotask(() => this.dispatchEvent(new Event(NativeAudio.failPrepare ? 'error' : 'canplay')));
  }
}
const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64), chimeEnabled: false, funEnabled: false };

describe('transaction audio explicit profile ownership', () => {
  beforeEach(() => {
    NativeAudio.owners = []; NativeAudio.failPrepare = false;
    vi.stubGlobal('window', { Audio: NativeAudio, speechSynthesis: { getVoices: () => [], speak: vi.fn(), cancel: vi.fn() } });
    vi.stubGlobal('fetch', vi.fn(async () => new Response('', { status: 404 })));
  });
  afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });
  it('requires confirmed server profile before preparation or playback', async () => {
    const engine = new TransactionAudioEngine({ isPublic: true });
    await expect(engine.speak({ text: '', transactionId: 'tx' })).rejects.toThrow('VOICE_PROFILE_UNAVAILABLE');
    expect(NativeAudio.owners).toHaveLength(0);
    engine.dispose();
  });
  it('prefetch really prepares media and speak consumes it without a second request', async () => {
    const engine = new TransactionAudioEngine({ isPublic: true }); engine.setProfile(profile);
    const onStart = vi.fn();
    const message = { text: '', amountVnd: '50000', transactionId: 'tx', onStart };
    const controller = new AbortController();
    await engine.prefetch(message, controller.signal);
    expect(NativeAudio.owners).toHaveLength(1);
    expect(NativeAudio.owners[0].play).not.toHaveBeenCalled();
    await engine.speak(message);
    expect(NativeAudio.owners).toHaveLength(1);
    expect(onStart).toHaveBeenCalledTimes(1);
    expect(NativeAudio.owners[0].stopped).toBe(true);
    engine.dispose();
  });
  it('binds native query to snapshot revision and never retries or falls back on Edge failure', async () => {
    NativeAudio.failPrepare = true;
    const onProfileChanged = vi.fn();
    const engine = new TransactionAudioEngine({ isPublic: true, onProfileChanged }); engine.setProfile(profile);
    await expect(engine.speak({ text: '', amountVnd: '50000', transactionId: 'tx' })).rejects.toThrow('AUDIO_PREPARATION_FAILED');
    expect(NativeAudio.owners).toHaveLength(1);
    expect(NativeAudio.owners[0].stopped).toBe(true);
    expect(window.speechSynthesis.speak).not.toHaveBeenCalled();
    expect(onProfileChanged).toHaveBeenCalledTimes(1);
    engine.dispose();
  });
  it('aborts prefetched native owner when profile changes, and dispose is idempotent', async () => {
    const engine = new TransactionAudioEngine({ isPublic: true }); engine.setProfile(profile);
    const next = { text: '', amountVnd: '50000', transactionId: 'tx' };
    await engine.prefetch(next, new AbortController().signal);
    engine.setProfile({ ...profile, revision: 2, edgeVoice: 'vi-VN-NamMinhNeural', synthesisKey: 'b'.repeat(64) });
    await Promise.resolve();
    expect(NativeAudio.owners[0].src).toBe('');
    engine.dispose(); engine.dispose();
    await expect(engine.speak(next)).rejects.toThrow('VOICE_DISPOSED');
  });
  it('never invents public replay route or sends client voice/text/amount overrides', () => {
    const replay = voiceRoute({ text: 'ignored', isReplay: true, transactionId: 't/x', phraseId: 'cute-small-o001-c001' }, true, profile, 'full');
    expect(replay.streamUrl).toContain('/voice/transactions/t%2Fx/stream?profileRevision=1');
    expect(replay.streamUrl).not.toContain('/replay');
    expect(replay.body).toEqual({ profileRevision: 1, phraseId: 'cute-small-o001-c001', part: 'full' });
    expect(voiceRoute({ text: '', transactionId: 'tx', phraseId: 'old' }, true, profile, 'amount').body.phraseId).toBeUndefined();
  });
});
