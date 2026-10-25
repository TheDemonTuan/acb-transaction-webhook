import { afterEach, beforeEach, describe, expect, it, vi } from 'bun:test';
import { setTestGlobal, restoreTestGlobals } from './test-globals';
import { TestAudioContext, TestNativeAudio, resetAudioTestFixture, CHIME_RECORDING_DURATION,
  chimeRecordingBuffer, chimeRecordingResponse } from './audio-test-fixture';
import { TransactionAudioEngine, scheduleChime, type TransactionAudioEngineOptions } from '../src/features/voice-announcements/transaction-audio-engine';
import { CHIME_ASSET_URL } from '../src/features/voice-announcements/chime-recording';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import { voiceRoute } from '../src/features/voice-announcements/announcement-plan';
import { AudioAssets } from '../src/features/voice-announcements/audio-assets';

const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64), chimeEnabled: false, funEnabled: false };
function stubAudioResponse(chosenProfile = profile) {
  const fetcher = vi.fn(async (url: string) => url.includes('/stream?')
    ? new Response(new Uint8Array([1, 2, 3]), { headers: {
      'X-Voice-Revision': String(chosenProfile.revision), 'X-Voice-Synthesis-Key': chosenProfile.synthesisKey,
      'X-TTS-Voice': chosenProfile.voiceId, 'X-TTS-Provider': 'vieneu', 'X-TTS-Fallback': 'false',
    } }) : url === CHIME_ASSET_URL ? chimeRecordingResponse() : new Response('', { status: 404 }));
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
    vi.spyOn(AudioAssets.prototype, 'preload').mockResolvedValue(undefined);
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
    const fetcher = stubAudioResponse(), engine = new TransactionAudioEngine({ isPublic: true }); engine.setProfile(profile);
    await expect(engine.speak({ text: '', transactionId: 'tx' })).rejects.toThrow('AUDIO_CONTEXT_NOT_RUNNING');
    expect(TestAudioContext.owners).toHaveLength(0); expect(fetcher).not.toHaveBeenCalled();
    expect(await engine.unlock()).toBe(true);
    expect(TestAudioContext.owners[0].resume).toHaveBeenCalledTimes(1);
    expect(TestAudioContext.owners[0].decodeAudioData).toHaveBeenCalledTimes(1);
    expect(fetcher.mock.calls[0][0]).toBe(CHIME_ASSET_URL);
    expect(AudioAssets.prototype.preload).toHaveBeenCalledTimes(1);
    await engine.speak({ text: '', transactionId: 'tx' });
    expect(fetcher).toHaveBeenCalledTimes(2); engine.dispose();
  });
  it('waits for both the bundled WAV response and decode before unlock succeeds', async () => {
    const response = Promise.withResolvers<Response>(), requested = Promise.withResolvers<void>();
    const decoded = Promise.withResolvers<AudioBuffer>(), decoding = Promise.withResolvers<void>();
    const fetcher = vi.fn((_url: string) => { requested.resolve(); return response.promise; });
    setTestGlobal('fetch', fetcher);
    const engine = createEngine(); engine.setProfile(profile);
    const context = TestAudioContext.owners[0], settled = vi.fn();
    context.decodeAudioData.mockImplementation(() => { decoding.resolve(); return decoded.promise; });
    const pending = engine.unlock().then((unlocked) => { settled(unlocked); return unlocked; });
    await requested.promise;
    expect(fetcher.mock.calls[0][0]).toBe(CHIME_ASSET_URL);
    expect(context.decodeAudioData).not.toHaveBeenCalled(); expect(settled).not.toHaveBeenCalled();
    response.resolve(chimeRecordingResponse());
    await decoding.promise;
    expect(settled).not.toHaveBeenCalled(); expect(context.sources).toHaveLength(0);
    decoded.resolve(chimeRecordingBuffer);
    expect(await pending).toBe(true);
    expect(settled).toHaveBeenCalledWith(true);
    expect(await engine.unlock()).toBe(true);
    expect(fetcher).toHaveBeenCalledTimes(1); expect(context.decodeAudioData).toHaveBeenCalledTimes(1);
    engine.dispose();
  });
  it.each(['network', 'http', 'decode'] as const)('reports unavailable after %s failure and retries only on re-unlock', async (failure) => {
    const onStatus = vi.fn(), engine = createEngine({ onStatus });
    engine.setProfile({ ...profile, chimeEnabled: true });
    const context = TestAudioContext.owners[0];
    const fetcher = vi.fn(async (_url: string) => chimeRecordingResponse());
    if (failure === 'network') fetcher.mockRejectedValueOnce(new Error('NETWORK_FAILURE'));
    if (failure === 'http') fetcher.mockResolvedValueOnce(new Response('', { status: 404 }));
    if (failure === 'decode') context.decodeAudioData.mockRejectedValueOnce(new Error('DECODE_FAILURE'));
    setTestGlobal('fetch', fetcher);
    expect(await engine.unlock()).toBe(false);
    expect(onStatus).toHaveBeenCalledWith('unavailable', 'CHIME_ASSET_UNAVAILABLE');
    expect(context.sources).toHaveLength(0); expect(context.gains).toHaveLength(0);
    await expect(engine.speak({ text: '', transactionId: 'tx', chime: true })).rejects.toThrow('CHIME_ASSET_UNAVAILABLE');
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(await engine.unlock()).toBe(true);
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(fetcher.mock.calls.every(([url]) => url === CHIME_ASSET_URL)).toBe(true);
    expect(engine.playChime()).toBeCloseTo(10 + CHIME_RECORDING_DURATION, 12);
    expect(context.sources[0].buffer).toBe(chimeRecordingBuffer);
    engine.dispose();
  });
  it.each(['cancel', 'dispose'] as const)('%s aborts an in-flight recording fetch and settles unlock false', async (operation) => {
    const requested = Promise.withResolvers<AbortSignal>();
    const fetcher = vi.fn((_url: string, init?: RequestInit) => {
      const signal = init?.signal as AbortSignal;
      requested.resolve(signal);
      const { promise, reject } = Promise.withResolvers<Response>();
      signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
      return promise;
    });
    setTestGlobal('fetch', fetcher);
    const engine = createEngine(); engine.setProfile(profile);
    const pending = engine.unlock(), signal = await requested.promise;
    engine[operation]();
    expect(await pending).toBe(false); expect(signal.aborted).toBe(true);
    const context = TestAudioContext.owners[0];
    expect(context.decodeAudioData).not.toHaveBeenCalled(); expect(context.sources).toHaveLength(0);
    if (operation === 'cancel') {
      stubAudioResponse();
      expect(await engine.unlock()).toBe(true);
    } else expect(await engine.unlock()).toBe(false);
    engine.dispose();
  });
  it('cancels a nonabortable recording decode immediately and ignores its late buffer after retry', async () => {
    const decoded = Promise.withResolvers<AudioBuffer>(), decoding = Promise.withResolvers<void>();
    const engine = createEngine(); engine.setProfile(profile);
    const context = TestAudioContext.owners[0];
    context.decodeAudioData.mockImplementationOnce(() => { decoding.resolve(); return decoded.promise; });
    const cancelled = engine.unlock();
    await decoding.promise; engine.cancel();
    expect(await cancelled).toBe(false);
    expect(context.sources).toHaveLength(0);
    expect(await engine.unlock()).toBe(true);
    decoded.resolve({ ...chimeRecordingBuffer });
    await decoded.promise;
    expect(engine.playChime()).toBeCloseTo(10 + CHIME_RECORDING_DURATION, 12);
    expect(context.sources).toHaveLength(1);
    expect(context.sources[0].buffer).toBe(chimeRecordingBuffer);
    engine.dispose();
  });
  it('does not fetch a recording from speech when unlock has not loaded one', async () => {
    const fetcher = stubAudioResponse(), engine = createEngine();
    engine.setProfile({ ...profile, chimeEnabled: true });
    await expect(engine.speak({ text: '', transactionId: 'tx', chime: true })).rejects.toThrow('CHIME_ASSET_UNAVAILABLE');
    expect(fetcher).not.toHaveBeenCalled();
    expect(TestAudioContext.owners[0].sources).toHaveLength(0);
    engine.dispose();
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
    expect(await engine.unlock()).toBe(true);
    const pending = engine.speak({ text: '', transactionId: 'tx', chime: true, onStart });
    const observed = pending.catch((error: unknown) => error);
    const audio = await loaded.promise;
    TestAudioContext.owners[0].setState('suspended');
    expect(await observed).toMatchObject({ message: 'AUDIO_CONTEXT_NOT_RUNNING', playbackStarted: false });
    expect(onStart).not.toHaveBeenCalled(); expect(audio.play).not.toHaveBeenCalled();
    const context = TestAudioContext.owners[0];
    expect(context.sources).toHaveLength(1);
    expect(context.sources[0].disconnect).toHaveBeenCalledTimes(1);
    expect(context.sources[0].stop).toHaveBeenCalledTimes(1);
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
  it.each([1, 0.5, 2])('owns the complete recording and disconnects its single source at volume %s', async (volume) => {
    const engine = createEngine(); engine.setProfile(profile);
    expect(await engine.unlock()).toBe(true);
    const end = engine.playChime(volume);
    expect(end).toBeCloseTo(10 + CHIME_RECORDING_DURATION, 12);
    const context = TestAudioContext.owners[0];
    expect(context.sources).toHaveLength(1); expect(context.gains).toHaveLength(1);
    const source = context.sources[0], gain = context.gains[0];
    expect(source.buffer).toBe(chimeRecordingBuffer);
    expect(source.start.mock.calls).toEqual([[10]]);
    expect(source.stop).not.toHaveBeenCalled();
    expect(source.playbackRate.value).toBe(1); expect(source.detune.value).toBe(0);
    expect(source.loop).toBe(false);
    expect(source.connect).toHaveBeenCalledWith(gain);
    expect(gain.connect).toHaveBeenCalledWith(context.destination);
    expect(context.shapers).toHaveLength(0);
    source.onended?.();
    expect(source.disconnect).toHaveBeenCalledTimes(1);
    expect(gain.disconnect).toHaveBeenCalledTimes(1);
    engine.dispose();
    expect(source.disconnect).toHaveBeenCalledTimes(1);
    expect(gain.disconnect).toHaveBeenCalledTimes(1);
  });
  it('schedules the supplied recording at its original rate on a BaseAudioContext', () => {
    const context = new TestAudioContext();
    const scheduled = scheduleChime(context as unknown as BaseAudioContext, 0.4, chimeRecordingBuffer);
    expect(scheduled.end).toBeCloseTo(10 + CHIME_RECORDING_DURATION, 12);
    expect(scheduled.nodes).toHaveLength(1);
    expect(scheduled.nodes[0].source).toBe(context.sources[0] as unknown as AudioBufferSourceNode);
    expect(scheduled.nodes[0].gain).toBe(context.gains[0] as unknown as GainNode);
    expect(context.sources[0].buffer).toBe(chimeRecordingBuffer);
    expect(context.sources[0].start.mock.calls).toEqual([[10]]);
    expect(context.sources[0].playbackRate.value).toBe(1);
    expect(context.sources[0].loop).toBe(false);
    context.sources[0].onended?.();
  });
  it('prepares during the recording but starts speech only after its source actually ends', async () => {
    vi.useFakeTimers();
    const loaded = Promise.withResolvers<TestNativeAudio>(); TestNativeAudio.onLoaded = loaded.resolve;
    const played = Promise.withResolvers<TestNativeAudio>(); TestNativeAudio.onPlayed = played.resolve;
    TestNativeAudio.autoEnded = false;
    const onStart = vi.fn(), settled = vi.fn();
    const engine = createEngine(); engine.setProfile({ ...profile, chimeEnabled: true });
    expect(await engine.unlock()).toBe(true);
    const pending = engine.speak({ text: '', transactionId: 'tx', chime: true, onStart }).then(settled);
    const audio = await loaded.promise;
    await Promise.resolve();
    const context = TestAudioContext.owners[0];
    expect(context.sources).toHaveLength(1);
    vi.advanceTimersByTime(10000);
    expect(audio.play).not.toHaveBeenCalled(); expect(onStart).not.toHaveBeenCalled();
    context.sources[0].onended?.();
    await played.promise;
    expect(audio.play).toHaveBeenCalledTimes(1); expect(onStart).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(10000);
    expect(settled).not.toHaveBeenCalled();
    audio.dispatchEvent(new Event('ended'));
    await pending;
    expect(audio.play).toHaveBeenCalledTimes(1); expect(onStart).toHaveBeenCalledTimes(1);
    engine.dispose();
  });
  it('cancels the recording source without starting prepared speech or accepting a late ended event', async () => {
    const loaded = Promise.withResolvers<TestNativeAudio>(); TestNativeAudio.onLoaded = loaded.resolve;
    const onStart = vi.fn();
    const engine = createEngine(); engine.setProfile({ ...profile, chimeEnabled: true });
    expect(await engine.unlock()).toBe(true);
    const pending = engine.speak({ text: '', transactionId: 'tx', chime: true, onStart });
    const observed = pending.catch((error: unknown) => error);
    const audio = await loaded.promise;
    const context = TestAudioContext.owners[0], source = context.sources[0], lateEnded = source.onended;
    engine.cancel();
    expect(await observed).toMatchObject({ message: 'VOICE_CANCELLED' });
    lateEnded?.();
    expect(audio.play).not.toHaveBeenCalled(); expect(onStart).not.toHaveBeenCalled();
    expect(source.onended).toBeNull(); expect(source.stop).toHaveBeenCalledTimes(1);
    expect(source.disconnect).toHaveBeenCalledTimes(1);
    expect(context.gains).toHaveLength(1);
    expect(context.gains[0].disconnect).toHaveBeenCalledTimes(1);
    engine.dispose();
  });
  it.each([0, -1])('creates no chime source or gain when muted at volume %s', async (volume) => {
    const engine = createEngine(); engine.setProfile(profile);
    expect(await engine.unlock()).toBe(true);
    expect(engine.playChime(volume)).toBe(0);
    const context = TestAudioContext.owners[0];
    expect(context.sources).toHaveLength(0);
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
