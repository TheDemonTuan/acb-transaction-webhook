import { afterEach, beforeEach, describe, expect, it, vi } from 'bun:test';
import { setTestGlobal, restoreTestGlobals } from './test-globals';
import { TestAudioContext, TestNativeAudio, resetAudioTestFixture } from './audio-test-fixture';
import { TransactionAudioEngine, type TransactionAudioEngineOptions } from '../src/features/voice-announcements/transaction-audio-engine';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import { voiceRoute } from '../src/features/voice-announcements/announcement-plan';
import { AudioAssets } from '../src/features/voice-announcements/audio-assets';

const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64), chimeEnabled: false, funEnabled: false };
function stubAudioResponse(chosenProfile = profile) {
  const fetcher = vi.fn(async (url: string) => url.includes('/stream?')
    ? new Response(new Uint8Array([1, 2, 3]), { headers: {
      'X-Voice-Revision': String(chosenProfile.revision), 'X-Voice-Synthesis-Key': chosenProfile.synthesisKey,
      'X-TTS-Voice': chosenProfile.voiceId, 'X-TTS-Provider': 'vieneu', 'X-TTS-Fallback': 'false',
    } }) : new Response('', { status: 404 }));
  setTestGlobal('fetch', fetcher); return fetcher;
}

function createEngine(options: TransactionAudioEngineOptions = {}) {
  const engine = new TransactionAudioEngine({ isPublic: true, ...options });
  Object.assign(engine, { audioContext: new TestAudioContext() });
  return engine;
}

describe('transaction audio explicit profile ownership', () => {
  beforeEach(() => {
    resetAudioTestFixture();
    setTestGlobal('window', { Audio: TestNativeAudio, AudioContext: TestAudioContext });
    setTestGlobal('MediaSource', undefined);
    stubAudioResponse();
  });
  afterEach(() => { vi.useRealTimers(); restoreTestGlobals(); vi.restoreAllMocks(); });
  it('requires confirmed server profile before preparation or playback', async () => {
    const engine = createEngine();
    await expect(engine.speak({ text: '', transactionId: 'tx' })).rejects.toThrow('VOICE_PROFILE_UNAVAILABLE');
    expect(TestNativeAudio.owners).toHaveLength(0);
    engine.dispose();
  });
  it('requires Web Audio support even when HTML audio exists', () => {
    setTestGlobal('window', { Audio: TestNativeAudio });
    const engine = new TransactionAudioEngine({ isPublic: true }); engine.setProfile(profile);
    expect(engine.isSupported()).toBe(false);
    engine.dispose();
  });
  it('creates and resumes the context only through unlock before speech can request audio', async () => {
    TestAudioContext.initialState = 'suspended';
    const preload = vi.spyOn(AudioAssets.prototype, 'preload').mockResolvedValue(undefined);
    const fetcher = stubAudioResponse(), engine = new TransactionAudioEngine({ isPublic: true }); engine.setProfile(profile);
    await expect(engine.speak({ text: '', transactionId: 'tx' })).rejects.toThrow('AUDIO_CONTEXT_NOT_RUNNING');
    expect(TestAudioContext.owners).toHaveLength(0); expect(fetcher).not.toHaveBeenCalled();
    expect(await engine.unlock()).toBe(true);
    expect(TestAudioContext.owners[0].resume).toHaveBeenCalledTimes(1);
    expect(preload).toHaveBeenCalledTimes(1);
    await engine.speak({ text: '', transactionId: 'tx' });
    expect(fetcher).toHaveBeenCalledTimes(1); engine.dispose();
  });
  it.each(['speak', 'prefetch'] as const)('rejects suspended %s without requesting audio or resuming outside unlock', async (operation) => {
    TestAudioContext.initialState = 'suspended';
    const fetcher = stubAudioResponse(), onStatus = vi.fn(), onStart = vi.fn();
    const engine = createEngine({ onStatus }); engine.setProfile(profile);
    const message = { text: '', amountVnd: '50000', transactionId: 'tx', onStart };
    const pending = operation === 'speak' ? engine.speak(message) : engine.prefetch(message, new AbortController().signal);
    await expect(pending).rejects.toMatchObject({ message: 'AUDIO_CONTEXT_NOT_RUNNING', playbackStarted: false });
    expect(fetcher).not.toHaveBeenCalled(); expect(onStart).not.toHaveBeenCalled();
    expect(TestAudioContext.owners[0].resume).not.toHaveBeenCalled();
    expect(onStatus).toHaveBeenCalledWith('unavailable', 'AUDIO_CONTEXT_NOT_RUNNING');
    engine.dispose();
  });
  it('prefetch really prepares media and speak consumes it without a second request or duplicate start', async () => {
    const fetcher = stubAudioResponse();
    const engine = createEngine(); engine.setProfile(profile);
    const onStart = vi.fn(), message = { text: '', amountVnd: '50000', transactionId: 'tx', onStart };
    const controller = new AbortController();
    await engine.prefetch(message, controller.signal);
    await engine.prefetch(message, controller.signal);
    expect(TestNativeAudio.owners).toHaveLength(1);
    expect(TestNativeAudio.owners[0].play).not.toHaveBeenCalled();
    await engine.speak(message);
    expect(fetcher.mock.calls.filter(([url]) => url.includes('/stream?'))).toHaveLength(1);
    expect(TestNativeAudio.owners).toHaveLength(1); expect(onStart).toHaveBeenCalledTimes(1);
    expect(TestNativeAudio.owners[0].play).toHaveBeenCalledTimes(1);
    expect(TestNativeAudio.owners[0].pause).toHaveBeenCalled();
    engine.dispose();
  });
  it('never retries or changes provider after native preparation fails', async () => {
    TestNativeAudio.failPrepare = true;
    const fetcher = stubAudioResponse(), onProfileChanged = vi.fn();
    const engine = createEngine({ onProfileChanged }); engine.setProfile(profile);
    await expect(engine.speak({ text: '', amountVnd: '50000', transactionId: 'tx' })).rejects.toThrow('AUDIO_PREPARATION_FAILED');
    expect(fetcher.mock.calls.filter(([url]) => url.includes('/stream?'))).toHaveLength(1);
    expect(TestNativeAudio.owners).toHaveLength(1); expect(TestNativeAudio.owners[0].pause).toHaveBeenCalled();
    expect(onProfileChanged).toHaveBeenCalledTimes(1);
    engine.dispose();
  });
  it.each(['speak', 'prefetch'] as const)('settles %s when context stops during preparation and aborts its request', async (operation) => {
    const requested = Promise.withResolvers<AbortSignal>();
    setTestGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
      if (!url.includes('/stream?')) return new Response('', { status: 404 });
      const signal = init.signal!;
      requested.resolve(signal);
      const { promise, reject } = Promise.withResolvers<Response>();
      signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
      return promise;
    }));
    const onStatus = vi.fn(), onStart = vi.fn();
    const engine = createEngine({ onStatus }); engine.setProfile(profile);
    const message = { text: '', transactionId: 'tx', onStart };
    const pending = operation === 'speak' ? engine.speak(message) : engine.prefetch(message, new AbortController().signal);
    const observed = pending.catch((error: unknown) => error);
    const requestSignal = await requested.promise;
    TestAudioContext.owners[0].setState('suspended');
    expect(await observed).toMatchObject({ message: 'AUDIO_CONTEXT_NOT_RUNNING', playbackStarted: false });
    expect(requestSignal.aborted).toBe(true); expect(onStart).not.toHaveBeenCalled();
    expect(onStatus).toHaveBeenCalledWith('unavailable', 'AUDIO_CONTEXT_NOT_RUNNING');
    engine.dispose();
  });
  it.each(['speak', 'prefetch'] as const)('settles %s before a nonabortable decode finishes and never retains its late buffer', async (operation) => {
    setTestGlobal('window', {});
    const decoded = Promise.withResolvers<AudioBuffer>(), decoding = Promise.withResolvers<void>();
    const retain = vi.spyOn(AudioAssets.prototype, 'retainDecoded'), onStart = vi.fn();
    const engine = createEngine(); engine.setProfile(profile);
    const context = TestAudioContext.owners[0];
    context.decodeAudioData.mockImplementation(() => { decoding.resolve(); return decoded.promise; });
    setTestGlobal('fetch', vi.fn(async () => new Response(new Uint8Array([1]), { headers: {
      'X-Voice-Revision': String(profile.revision), 'X-Voice-Synthesis-Key': profile.synthesisKey,
      'X-TTS-Voice': profile.voiceId, 'X-TTS-Provider': 'vieneu', 'X-TTS-Fallback': 'false',
    } })));
    const message = { text: '', transactionId: 'tx', onStart };
    const pending = operation === 'speak' ? engine.speak(message) : engine.prefetch(message, new AbortController().signal);
    const observed = pending.catch((error: unknown) => error);
    await decoding.promise; context.setState('suspended');
    expect(await observed).toMatchObject({ message: 'AUDIO_CONTEXT_NOT_RUNNING', playbackStarted: false });
    decoded.resolve({ duration: 1, length: 24000, numberOfChannels: 1 } as AudioBuffer);
    await decoded.promise;
    expect(retain).not.toHaveBeenCalled(); expect(onStart).not.toHaveBeenCalled();
    expect(context.sources).toHaveLength(0); engine.dispose();
  });
  it('settles context loss during the chime without reporting speech started', async () => {
    const loaded = Promise.withResolvers<TestNativeAudio>(); TestNativeAudio.onLoaded = loaded.resolve;
    const onStart = vi.fn(), onStatus = vi.fn();
    const engine = createEngine({ onStatus }); engine.setProfile({ ...profile, chimeEnabled: true });
    const pending = engine.speak({ text: '', transactionId: 'tx', chime: true, onStart });
    const observed = pending.catch((error: unknown) => error);
    const audio = await loaded.promise;
    TestAudioContext.owners[0].setState('suspended');
    expect(await observed).toMatchObject({ message: 'AUDIO_CONTEXT_NOT_RUNNING', playbackStarted: false });
    expect(onStart).not.toHaveBeenCalled(); expect(audio.play).not.toHaveBeenCalled();
    expect(TestAudioContext.owners[0].oscillators.every((node) => node.disconnect.mock.calls.length > 0)).toBe(true);
    expect(onStatus).toHaveBeenCalledWith('unavailable', 'AUDIO_CONTEXT_NOT_RUNNING');
    engine.dispose();
  });
  it.each([false, true])('settles media context loss with playbackStarted=%s only after onStart', async (started) => {
    TestNativeAudio.autoPlaying = started; TestNativeAudio.autoEnded = false;
    const played = Promise.withResolvers<TestNativeAudio>(); TestNativeAudio.onPlayed = played.resolve;
    const onStart = vi.fn(), onStatus = vi.fn();
    const engine = createEngine({ onStatus }); engine.setProfile(profile);
    const pending = engine.speak({ text: '', transactionId: 'tx', onStart });
    const observed = pending.catch((error: unknown) => error);
    const audio = await played.promise;
    TestAudioContext.owners[0].setState('suspended');
    expect(await observed).toMatchObject({ message: 'AUDIO_CONTEXT_NOT_RUNNING', playbackStarted: started });
    expect(onStart).toHaveBeenCalledTimes(started ? 1 : 0);
    audio.dispatchEvent(new Event('playing')); audio.dispatchEvent(new Event('ended'));
    expect(onStart).toHaveBeenCalledTimes(started ? 1 : 0);
    expect(audio.pause).toHaveBeenCalled();
    expect(onStatus).toHaveBeenCalledWith('unavailable', 'AUDIO_CONTEXT_NOT_RUNNING');
    engine.dispose();
  });
  it.each([false, true])('maps a real media NotAllowedError with playbackStarted=%s without retry', async (started) => {
    const denied = new DOMException('Gesture required', 'NotAllowedError');
    if (started) TestNativeAudio.onPlayed = () => { throw denied; };
    else TestNativeAudio.playError = denied;
    const onStart = vi.fn(), onStatus = vi.fn(), fetcher = stubAudioResponse();
    const engine = createEngine({ onStatus }); engine.setProfile(profile);
    await expect(engine.speak({ text: '', transactionId: 'tx', onStart })).rejects.toMatchObject({ message: 'AUDIO_CONTEXT_NOT_RUNNING', playbackStarted: started });
    expect(onStart).toHaveBeenCalledTimes(started ? 1 : 0);
    expect(onStatus).toHaveBeenCalledWith('unavailable', 'AUDIO_CONTEXT_NOT_RUNNING');
    expect(fetcher).toHaveBeenCalledTimes(1); engine.dispose();
  });
  it('cancel releases media and ignores late playing events without poisoning the next announcement', async () => {
    TestNativeAudio.autoPlaying = false; TestNativeAudio.autoEnded = false;
    const played = Promise.withResolvers<TestNativeAudio>(); TestNativeAudio.onPlayed = played.resolve;
    const onStart = vi.fn(), engine = createEngine(); engine.setProfile(profile);
    const pending = engine.speak({ text: '', transactionId: 'tx', onStart });
    const observed = pending.catch((error: unknown) => error), audio = await played.promise;
    engine.cancel();
    expect(await observed).toMatchObject({ message: 'VOICE_CANCELLED', playbackStarted: false });
    audio.dispatchEvent(new Event('playing')); audio.dispatchEvent(new Event('ended'));
    expect(onStart).not.toHaveBeenCalled(); expect(audio.src).toBe('');
    TestNativeAudio.autoPlaying = true; TestNativeAudio.autoEnded = true;
    await engine.speak({ text: '', transactionId: 'next', onStart });
    expect(onStart).toHaveBeenCalledTimes(1); engine.dispose();
  });
  it('does not let a cancelled old prefetch release its immediately installed replacement', async () => {
    TestNativeAudio.autoCanPlay = false;
    const loaded = Promise.withResolvers<TestNativeAudio>(); TestNativeAudio.onLoaded = loaded.resolve;
    const fetcher = stubAudioResponse(), engine = createEngine(); engine.setProfile(profile);
    const oldAbort = new AbortController();
    const old = engine.prefetch({ text: '', transactionId: 'old' }, oldAbort.signal);
    const observedOld = old.catch((error: unknown) => error);
    const oldAudio = await loaded.promise;
    oldAbort.abort();
    TestNativeAudio.autoCanPlay = true;
    const message = { text: '', transactionId: 'replacement', onStart: vi.fn() };
    const replacement = engine.prefetch(message, new AbortController().signal);
    expect(await observedOld).toMatchObject({ message: 'VOICE_CANCELLED' });
    await replacement;
    expect(oldAudio.src).toBe('');
    const replacementAudio = TestNativeAudio.owners[1];
    expect(replacementAudio.src).not.toBe('');
    await engine.speak(message);
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(TestNativeAudio.owners).toHaveLength(2);
    expect(replacementAudio.play).toHaveBeenCalledTimes(1);
    expect(message.onStart).toHaveBeenCalledTimes(1); engine.dispose();
  });
  it('aborts prefetched native owner when profile changes, and dispose is idempotent', async () => {
    const engine = createEngine(); engine.setProfile(profile);
    const next = { text: '', amountVnd: '50000', transactionId: 'tx' };
    await engine.prefetch(next, new AbortController().signal);
    engine.setProfile({ ...profile, revision: 2, voiceId: 'vieneu-other', synthesisKey: 'b'.repeat(64) });
    expect(TestNativeAudio.owners[0].src).toBe('');
    engine.dispose(); engine.dispose();
    await expect(engine.speak(next)).rejects.toThrow('VOICE_DISPOSED');
  });
  it.each([1, 0.5])('schedules the 1200ms overlapping chime with proportional timing and volume %s', (volume) => {
    const engine = createEngine(); engine.setProfile(profile);
    expect(engine.playChime(volume)).toBeCloseTo(11.2, 12);
    const context = TestAudioContext.owners[0], scale = 1200 / 850;
    expect(context.oscillators).toHaveLength(2);
    expect(context.gains).toHaveLength(2);
    for (const [index, [frequency, offset, duration]] of [[880, 0, 0.40], [1320, 0.30, 0.55]].entries()) {
      const oscillator = context.oscillators[index], gain = context.gains[index];
      const start = 10 + offset * scale, end = start + duration * scale;
      expect(oscillator.type).toBe('sine');
      expect(oscillator.frequency.setValueAtTime).toHaveBeenCalledTimes(1);
      expect(oscillator.frequency.setValueAtTime.mock.calls[0][0]).toBe(frequency);
      expect(oscillator.frequency.setValueAtTime.mock.calls[0][1]).toBeCloseTo(start, 12);
      expect(oscillator.start).toHaveBeenCalledTimes(1);
      expect(oscillator.start.mock.calls[0][0]).toBeCloseTo(start, 12);
      expect(oscillator.stop).toHaveBeenCalledTimes(1);
      expect(oscillator.stop.mock.calls[0][0]).toBeCloseTo(end, 12);
      expect(gain.gain.setValueAtTime).toHaveBeenCalledTimes(1);
      expect(gain.gain.setValueAtTime.mock.calls[0][0]).toBe(0.0001);
      expect(gain.gain.setValueAtTime.mock.calls[0][1]).toBeCloseTo(start, 12);
      const ramps = gain.gain.exponentialRampToValueAtTime.mock.calls;
      expect(ramps.map(([value]) => value)).toEqual([0.300 * volume, 0.080 * volume, 0.0001]);
      expect(ramps[0][1]).toBeCloseTo(start + 0.01 * scale, 12);
      expect(ramps[1][1]).toBeCloseTo(start + duration * scale * 0.5, 12);
      expect(ramps[2][1]).toBeCloseTo(end, 12);
      expect(oscillator.connect).toHaveBeenCalledWith(gain);
      expect(gain.connect).toHaveBeenCalledWith(context.destination);
    }
    expect(context.oscillators.every((node) => !node.connect.mock.calls.some(([target]) => target === context.destination))).toBe(true);
    engine.dispose();
  });
  it('creates no chime oscillator or gain when muted', () => {
    const engine = createEngine(); engine.setProfile(profile);
    expect(engine.playChime(0)).toBe(0);
    const context = TestAudioContext.owners[0];
    expect(context.oscillators).toHaveLength(0);
    expect(context.gains).toHaveLength(0);
    engine.dispose();
  });
  it('never invents public replay route or sends client voice/text/amount overrides', () => {
    const replay = voiceRoute({ text: 'ignored', isReplay: true, transactionId: 't/x', phraseId: 'cute-small-o001-c001' }, true, profile, 'full');
    expect(replay.streamUrl).toContain('/voice/transactions/t%2Fx/stream?profileRevision=1');
    expect(replay.streamUrl).not.toContain('/replay');
    expect(replay.body).toEqual({ profileRevision: 1, phraseId: 'cute-small-o001-c001', part: 'full' });
    expect(voiceRoute({ text: '', transactionId: 'tx', phraseId: 'old' }, true, profile, 'amount').body.phraseId).toBeUndefined();
  });
});
