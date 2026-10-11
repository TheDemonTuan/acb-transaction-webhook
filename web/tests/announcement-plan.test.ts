import { afterEach, describe, expect, it, vi } from 'vitest';
import { prepareAnnouncement, playPrepared, type PreparedAnnouncement, type PreparationOptions } from '../src/features/voice-announcements/announcement-plan';
import { AudioAssets } from '../src/features/voice-announcements/audio-assets';
import { BrowserSpeechEngine } from '../src/features/voice-announcements/browser-speech-engine';
import { RecentPhraseRing } from '../src/features/voice-announcements/voice-copy';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import * as catalogModule from '../src/features/voice-announcements/voice-catalog';

interface TestSource { buffer: AudioBuffer | null; playbackRate: { value: number }; start: (time: number) => void;
  stop: () => void; disconnect: () => void; connect: () => void; onended: (() => void) | null }
function fixture() {
  const starts: number[] = [], sources: TestSource[] = [];
  const context = { currentTime: 10, state: 'running', destination: {}, createGain: () => ({ gain: { setValueAtTime: vi.fn() }, connect: vi.fn(), disconnect: vi.fn() }),
    createBufferSource: () => {
      const source: TestSource = { buffer: null, playbackRate: { value: 1 }, start: (time) => { starts.push(time); },
        stop: vi.fn(), disconnect: vi.fn(), connect: vi.fn(), onended: null };
      sources.push(source); return source;
    } };
  const options: PreparationOptions = { isPublic: true, context: context as unknown as AudioContext,
    assets: new AudioAssets(true), recent: new RecentPhraseRing(), browser: new BrowserSpeechEngine() };
  return { context, options, starts, sources };
}
const buffer = (duration: number) => ({ duration }) as AudioBuffer;
const recipe = { id: 'cute-small-o001-c001', styleId: 'cute', bandId: 'small', weight: 1,
  openingId: 'o', closingId: 'c', tail: 'Cảm ơn quý khách.', fragmentIds: ['o', 'c'] };

describe('prepared audio timeline', () => {
  afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
  it('schedules complete core and approved tail buffers on one audio clock after chime', async () => {
    vi.useFakeTimers();
    const f = fixture(), onTailStart = vi.fn(), onStart = vi.fn();
    const plan: PreparedAnnouncement = { kind: 'buffers', profile: DEFAULT_VOICE_PROFILE, volume: 1, recipe,
      amountBuffer: buffer(2), tailBuffers: [buffer(1), buffer(0.5)], message: { text: '', onStart, onTailStart }, release: vi.fn() };
    const pending = playPrepared(plan, new AbortController().signal, f.options, 10.25);
    expect(f.starts).toEqual([10.25, 12.25, 13.25]);
    expect(f.sources.every((source) => source.playbackRate.value === 1)).toBe(true);
    await vi.advanceTimersByTimeAsync(250);
    expect(onStart).toHaveBeenCalledTimes(1);
    expect(onTailStart).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(2000);
    expect(onTailStart).toHaveBeenCalledWith(recipe.id);
    f.sources.at(-1)!.onended!(); await pending;
    expect(f.sources.every((source) => vi.mocked(source.disconnect).mock.calls.length === 1)).toBe(true);
  });
  it('schedules ready cold whole-tail before core end and drops late tail without rereading amount', async () => {
    vi.useFakeTimers();
    const f = fixture(), onTailStart = vi.fn(), abortTail = vi.fn();
    const tail = Promise.withResolvers<AudioBuffer | undefined>();
    const plan: PreparedAnnouncement = { kind: 'buffers', profile: DEFAULT_VOICE_PROFILE, volume: 1, recipe,
      amountBuffer: buffer(2), tailBuffers: [], tail: { ready: tail.promise, abort: abortTail },
      message: { text: '', onTailStart }, release: vi.fn() };
    const pending = playPrepared(plan, new AbortController().signal, f.options);
    tail.resolve(buffer(1)); await Promise.resolve();
    expect(f.starts).toEqual([10, 12]);
    f.sources[0].onended!();
    f.sources[1].onended!(); await pending;
    const late = fixture(), delayed = Promise.withResolvers<AudioBuffer | undefined>();
    const lateAbort = vi.fn();
    const next = playPrepared({ ...plan, tail: { ready: delayed.promise, abort: lateAbort } }, new AbortController().signal, late.options);
    late.context.currentTime = 12;
    late.sources[0].onended!(); await next;
    delayed.resolve(buffer(1)); await Promise.resolve();
    expect(late.starts).toEqual([10]); expect(lateAbort).toHaveBeenCalledTimes(1);
  });
  it('cancel stops all nodes and settles once without late start or tail callbacks', async () => {
    vi.useFakeTimers();
    const f = fixture(), onStart = vi.fn(), onTailStart = vi.fn(), controller = new AbortController();
    const plan: PreparedAnnouncement = { kind: 'buffers', profile: DEFAULT_VOICE_PROFILE, volume: 1, recipe,
      amountBuffer: buffer(2), tailBuffers: [buffer(1)], message: { text: '', onStart, onTailStart }, release: vi.fn() };
    const pending = playPrepared(plan, controller.signal, f.options, 10.25);
    controller.abort();
    await expect(pending).rejects.toMatchObject({ message: 'VOICE_CANCELLED', playbackStarted: false });
    await vi.advanceTimersByTimeAsync(5000);
    expect(onStart).not.toHaveBeenCalled(); expect(onTailStart).not.toHaveBeenCalled();
    expect(f.sources.every((source) => vi.mocked(source.stop).mock.calls.length === 1)).toBe(true);
  });

  it('colliding current and next seeds reserve distinct recipes and release on cancellation or preparation failure', async () => {
    const f = fixture();
    const profile = { ...DEFAULT_VOICE_PROFILE, providerMode: 'BROWSER_ONLY' as const,
      amountBands: DEFAULT_VOICE_PROFILE.amountBands.map((band) => ({ ...band, styleId: 'cute' as const })) };
    vi.spyOn(catalogModule, 'loadVoiceCatalog').mockResolvedValue({ version: 'v1', amountCore: 'Đã nhận {amount}.',
      fragments: [{ id: 'o', text: 'Niềm vui vừa ghé.', styleId: 'cute', bandId: 'small' },
        { id: 'c', text: 'Cảm ơn quý khách.', styleId: 'cute', bandId: 'small' }], classic: [],
      recipes: [{ ...recipe, id: 'first' }, { ...recipe, id: 'second' }] });
    vi.stubGlobal('window', { speechSynthesis: { getVoices: () => [{ voiceURI: 'exact', lang: 'vi-VN' }], cancel: vi.fn() }, SpeechSynthesisUtterance: class {} });
    const firstAbort = new AbortController(), nextAbort = new AbortController();
    const [first, next] = await Promise.all([
      prepareAnnouncement({ text: '', transactionId: 'same-seed', amountVnd: '1000', voiceURI: 'exact' }, profile, firstAbort.signal, f.options),
      prepareAnnouncement({ text: '', transactionId: 'same-seed', amountVnd: '1000', voiceURI: 'exact' }, profile, nextAbort.signal, f.options),
    ]);
    expect(first.recipe!.id).not.toBe(next.recipe!.id);
    expect(f.options.recent.snapshot()).toHaveLength(2);
    firstAbort.abort(); next.release();
    expect(f.options.recent.snapshot()).toEqual([]);
    await expect(prepareAnnouncement({ text: '', transactionId: 'same-seed', amountVnd: '1000', voiceURI: 'missing' }, profile,
      new AbortController().signal, f.options)).rejects.toThrow('BROWSER_VIETNAMESE_VOICE_UNAVAILABLE');
    expect(f.options.recent.snapshot()).toEqual([]);
    f.options.assets.dispose(); f.options.browser.dispose();
  });
});
