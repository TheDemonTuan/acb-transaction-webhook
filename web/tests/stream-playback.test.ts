import { afterEach, beforeEach, describe, expect, it, vi } from 'bun:test';
import { setTestGlobal, restoreTestGlobals } from './test-globals';
import { TestNativeAudio, resetAudioTestFixture } from './audio-test-fixture';
import { prepareStreamPlayback } from '../src/features/voice-announcements/stream-playback';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';

const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64), coldRequestTimeoutMs: 8000 };
const headers = {
  'X-Voice-Revision': String(profile.revision), 'X-Voice-Synthesis-Key': profile.synthesisKey,
  'X-TTS-Voice': profile.voiceId, 'X-TTS-Provider': 'vieneu', 'X-TTS-Fallback': 'false',
};

class TestSourceBuffer extends EventTarget {
  updating = false;
  appended = Promise.withResolvers<void>();
  appendBuffer = vi.fn(() => {
    this.updating = true;
    queueMicrotask(() => { this.updating = false; this.dispatchEvent(new Event('updateend')); this.appended.resolve(); });
  });
}
class TestMediaSource extends EventTarget {
  static owners: TestMediaSource[] = [];
  static isTypeSupported = () => true;
  buffer = new TestSourceBuffer();
  eof = Promise.withResolvers<void>();
  readyState = 'open';
  constructor() { super(); TestMediaSource.owners.push(this); }
  addSourceBuffer = vi.fn(() => this.buffer);
  endOfStream = vi.fn(() => { this.eof.resolve(); });
}

function transport(mse: boolean) {
  setTestGlobal('MediaSource', mse ? TestMediaSource : undefined);
  let body!: ReadableStreamDefaultController<Uint8Array>;
  const cancelled = vi.fn();
  const stream = new ReadableStream<Uint8Array>({ start(controller) { body = controller; }, cancel: cancelled });
  const fetcher = vi.fn(async (_url: string, init?: RequestInit) => {
    init?.signal?.addEventListener('abort', () => body.error(new DOMException('Aborted', 'AbortError')), { once: true });
    return new Response(stream, { headers });
  });
  setTestGlobal('fetch', fetcher);
  const audio = new TestNativeAudio(), controller = new AbortController();
  const pending = prepareStreamPlayback('/voice/stream', profile, controller.signal, audio as unknown as HTMLAudioElement);
  return { audio, controller, pending, fetcher, cancelled, body: () => body };
}

describe('stream playback readiness and ownership', () => {
  beforeEach(() => {
    resetAudioTestFixture(); TestNativeAudio.autoCanPlay = false; TestMediaSource.owners = [];
    vi.spyOn(URL, 'createObjectURL').mockImplementation((value) => {
      if (value instanceof TestMediaSource) queueMicrotask(() => value.dispatchEvent(new Event('sourceopen')));
      return 'blob:voice-test';
    });
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  });
  afterEach(() => { vi.useRealTimers(); restoreTestGlobals(); vi.restoreAllMocks(); });

  it.each([false, true])('does not resolve at EOF before canplay (MediaSource=%s)', async (mse) => {
    const loaded = Promise.withResolvers<TestNativeAudio>(); TestNativeAudio.onLoaded = loaded.resolve;
    const f = transport(mse);
    let settled = false;
    void f.pending.then(() => { settled = true; }, () => { settled = true; });
    f.body().enqueue(new Uint8Array([1, 2, 3])); f.body().close();
    await loaded.promise;
    if (mse) await TestMediaSource.owners[0].eof.promise;
    expect(settled).toBe(false);
    f.audio.dispatchEvent(new Event('canplay'));
    const playback = await f.pending; await playback.completed;
    expect(settled).toBe(true); expect(f.fetcher).toHaveBeenCalledTimes(1);
    playback.release(); playback.release();
    expect(f.audio.src).toBe(''); expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1);
  });

  it('keeps the eight-second deadline after canplay until the stream reaches EOF', async () => {
    vi.useFakeTimers();
    const f = transport(true);
    // Enqueuing a chunk lets the test observe installed MSE readiness listeners.
    const created = Promise.withResolvers<void>();
    TestNativeAudio.onLoaded = () => created.resolve();
    f.body().enqueue(new Uint8Array([1, 2, 3]));
    await created.promise;
    const media = TestMediaSource.owners[0]; await media.buffer.appended.promise;
    f.audio.dispatchEvent(new Event('canplay'));
    const playback = await f.pending;
    let completed = false;
    const observed = playback.completed.catch((error: unknown) => { completed = true; return error; });
    const signal = f.fetcher.mock.calls[0][1]!.signal!;
    vi.advanceTimersByTime(7999);
    expect(signal.aborted).toBe(false); expect(completed).toBe(false);
    vi.advanceTimersByTime(1);
    expect(await observed).toMatchObject({ message: 'TTS_TIMEOUT' });
    expect(signal.aborted).toBe(true); expect(f.audio.src).toBe('');
    expect(f.audio.pause).toHaveBeenCalled(); expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1);
  });

  it('clears the deadline only after both canplay and EOF, never timing out subsequent playback', async () => {
    vi.useFakeTimers();
    const created = Promise.withResolvers<void>(); TestNativeAudio.onLoaded = () => created.resolve();
    const f = transport(true); f.body().enqueue(new Uint8Array([1]));
    await created.promise;
    const media = TestMediaSource.owners[0]; await media.buffer.appended.promise;
    f.audio.dispatchEvent(new Event('canplay')); const playback = await f.pending;
    f.body().close(); await playback.completed;
    vi.advanceTimersByTime(8000);
    expect(f.fetcher.mock.calls[0][1]!.signal!.aborted).toBe(false);
    expect(f.audio.src).toBe('blob:voice-test'); playback.release();
  });

  it.each([false, true])('times out EOF-without-canplay at eight seconds and releases (MediaSource=%s)', async (mse) => {
    vi.useFakeTimers();
    const loaded = Promise.withResolvers<void>(); TestNativeAudio.onLoaded = () => loaded.resolve();
    const f = transport(mse), observed = f.pending.catch((error: unknown) => error);
    f.body().enqueue(new Uint8Array([1])); f.body().close(); await loaded.promise;
    if (mse) await TestMediaSource.owners[0].eof.promise;
    vi.advanceTimersByTime(8000);
    expect(await observed).toMatchObject({ message: 'TTS_TIMEOUT' });
    expect(f.audio.src).toBe(''); expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1);
  });

  it.each([false, true])('abort before readiness settles promptly and releases (MediaSource=%s)', async (mse) => {
    const loaded = Promise.withResolvers<void>(); TestNativeAudio.onLoaded = () => loaded.resolve();
    const f = transport(mse), observed = f.pending.catch((error: unknown) => error);
    f.body().enqueue(new Uint8Array([1]));
    if (!mse) f.body().close();
    await loaded.promise; f.controller.abort();
    expect(await observed).toMatchObject({ message: 'VOICE_CANCELLED' });
    expect(f.audio.src).toBe(''); expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1);
    f.audio.dispatchEvent(new Event('canplay'));
    expect(f.fetcher).toHaveBeenCalledTimes(1);
  });

  it('late body failure after canplay rejects completed and releases transport without retry', async () => {
    const loaded = Promise.withResolvers<void>(); TestNativeAudio.onLoaded = () => loaded.resolve();
    const f = transport(true); f.body().enqueue(new Uint8Array([1])); await loaded.promise;
    await TestMediaSource.owners[0].buffer.appended.promise;
    f.audio.dispatchEvent(new Event('canplay')); const playback = await f.pending;
    const observed = playback.completed.catch((error: unknown) => error);
    f.body().error(new Error('STREAM_BODY_FAILURE'));
    expect(await observed).toMatchObject({ message: 'STREAM_BODY_FAILURE' });
    expect(f.audio.pause).toHaveBeenCalled(); expect(f.audio.src).toBe('');
    expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1); expect(f.fetcher).toHaveBeenCalledTimes(1);
    playback.release(); expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1);
  });

  it.each([
    { 'X-TTS-Provider': 'piper' }, { 'X-TTS-Fallback': 'true' }, { 'X-TTS-Voice': 'other' },
    { 'X-Voice-Revision': '2' }, { 'X-Voice-Synthesis-Key': 'b'.repeat(64) },
  ])('rejects unapproved response metadata before media setup: %j', async (override) => {
    setTestGlobal('MediaSource', TestMediaSource);
    const cancel = vi.fn(), body = new ReadableStream<Uint8Array>({ cancel });
    const fetcher = vi.fn(async () => new Response(body, { headers: { ...headers, ...override } })); setTestGlobal('fetch', fetcher);
    const audio = new TestNativeAudio();
    await expect(prepareStreamPlayback('/voice/stream', profile, new AbortController().signal, audio as unknown as HTMLAudioElement))
      .rejects.toThrow('VOICE_AUDIO_PROFILE_MISMATCH');
    expect(cancel).toHaveBeenCalledTimes(1); expect(URL.createObjectURL).not.toHaveBeenCalled();
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});
