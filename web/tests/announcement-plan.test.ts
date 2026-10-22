import { afterEach, describe, expect, it, vi, type Mock } from 'bun:test';
import { setTestGlobal, restoreTestGlobals } from './test-globals';
import { prepareAnnouncement, playPrepared, type PreparedAnnouncement, type PreparationOptions } from '../src/features/voice-announcements/announcement-plan';
import { AudioAssets } from '../src/features/voice-announcements/audio-assets';
import { BrowserSpeechEngine } from '../src/features/voice-announcements/browser-speech-engine';
import { RecentPhraseRing } from '../src/features/voice-announcements/voice-copy';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import * as catalogModule from '../src/features/voice-announcements/voice-catalog';

interface TestSource { buffer: AudioBuffer | null; playbackRate: { value: number }; start: (time: number) => void;
  stop: Mock<() => void>; disconnect: Mock<() => void>; connect: Mock<() => void>; onended: (() => void) | null }
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
  afterEach(() => { vi.useRealTimers(); restoreTestGlobals(); vi.restoreAllMocks(); });
  it('schedules complete core and approved tail buffers on one audio clock after chime', async () => {
    vi.useFakeTimers();
    const f = fixture(), onTailStart = vi.fn(), onStart = vi.fn();
    const plan: PreparedAnnouncement = { kind: 'buffers', profile: DEFAULT_VOICE_PROFILE, volume: 1, recipe,
      segments: [{ buffer: buffer(2), role: 'amount' }, { buffer: buffer(1), role: 'tail' }, { buffer: buffer(0.5), role: 'tail' }], message: { text: '', onStart, onTailStart }, release: vi.fn() };
    const pending = playPrepared(plan, new AbortController().signal, f.options, 10.25);
    expect(f.starts).toEqual([10.25, 12.25, 13.25]);
    expect(f.sources.every((source) => source.playbackRate.value === 1)).toBe(true);
    vi.advanceTimersByTime(250);
    expect(onStart).toHaveBeenCalledTimes(1);
    expect(onTailStart).not.toHaveBeenCalled();
    vi.advanceTimersByTime(2000);
    expect(onTailStart).toHaveBeenCalledWith(recipe.id);
    f.sources.at(-1)!.onended!(); await pending;
    expect(f.sources.every((source) => source.disconnect.mock.calls.length === 1)).toBe(true);
  });
  it('schedules ordered segments on timeline and triggers onTailStart when tail begins', async () => {
    vi.useFakeTimers();
    const f = fixture(), onTailStart = vi.fn();
    const plan: PreparedAnnouncement = {
      kind: 'buffers', profile: DEFAULT_VOICE_PROFILE, volume: 1, recipe,
      segments: [
        { buffer: buffer(2), role: 'amount' },
        { buffer: buffer(1), role: 'tail', clipId: recipe.id },
      ],
      message: { text: '', onTailStart },
      release: vi.fn(),
    };
    const pending = playPrepared(plan, new AbortController().signal, f.options, 10);
    expect(f.starts).toEqual([10, 12]);
    vi.advanceTimersByTime(2000);
    expect(onTailStart).toHaveBeenCalledWith(recipe.id);
    f.sources[1].onended!();
    await pending;
  });
  it('cancel stops all nodes and settles once without late start or tail callbacks', async () => {
    vi.useFakeTimers();
    const f = fixture(), onStart = vi.fn(), onTailStart = vi.fn(), controller = new AbortController();
    const plan: PreparedAnnouncement = { kind: 'buffers', profile: DEFAULT_VOICE_PROFILE, volume: 1, recipe,
      segments: [{ buffer: buffer(2), role: 'amount' }, { buffer: buffer(1), role: 'tail' }], message: { text: '', onStart, onTailStart }, release: vi.fn() };
    const pending = playPrepared(plan, controller.signal, f.options, 10.25);
    controller.abort();
    await expect(pending).rejects.toMatchObject({ message: 'VOICE_CANCELLED', playbackStarted: false });
    vi.advanceTimersByTime(5000);
    expect(onStart).not.toHaveBeenCalled(); expect(onTailStart).not.toHaveBeenCalled();
    expect(f.sources.every((source) => source.stop.mock.calls.length === 1)).toBe(true);
  });

  it('colliding current and next seeds reserve distinct recipes and release on cancellation or preparation failure', async () => {
    const f = fixture();
    const profile = { ...DEFAULT_VOICE_PROFILE, providerMode: 'BROWSER_ONLY' as const,
      amountBands: DEFAULT_VOICE_PROFILE.amountBands.map((band) => ({ ...band, styleId: 'cute' as const })) };
    vi.spyOn(catalogModule, 'loadVoiceCatalog').mockResolvedValue({ version: 'v1', amountCore: 'Đã nhận {amount}.',
      fragments: [{ id: 'o', text: 'Niềm vui vừa ghé.', styleId: 'cute', bandId: 'small' },
        { id: 'c', text: 'Cảm ơn quý khách.', styleId: 'cute', bandId: 'small' }], classic: [],
      recipes: [{ ...recipe, id: 'first' }, { ...recipe, id: 'second' }] });
    setTestGlobal('window', { speechSynthesis: { getVoices: () => [{ voiceURI: 'exact', lang: 'vi-VN' }], cancel: vi.fn() }, SpeechSynthesisUtterance: class {} });
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

describe('server-provider audio preparation', () => {
  afterEach(() => { restoreTestGlobals(); vi.restoreAllMocks(); });
  const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64), funEnabled: false };
  const decoded = { duration: 1, length: 24000, numberOfChannels: 1 } as AudioBuffer;
  const responseHeaders = (p: typeof profile = profile, provider = 'piper') => ({
    'Content-Type': 'audio/mpeg', 'X-TTS-Provider': provider, 'X-TTS-Voice': p.voiceId,
    'X-Voice-Revision': String(p.revision), 'X-Voice-Synthesis-Key': p.synthesisKey,
  });

  it.each([
    { ...profile, providerMode: 'PIPER_ONLY' as const, provider: 'piper' },
    { ...profile, providerMode: 'EDGE_ONLY' as const, voiceId: 'vi-VN-NamMinhNeural', provider: 'edge' },
  ])('verifies $providerMode buffered metadata and leaves playback rate neutral', async (chosen) => {
    setTestGlobal('window', {});
    const f = fixture();
    vi.spyOn(f.options.assets, 'getManifest').mockResolvedValue(null);
    const decode = vi.fn(async () => decoded);
    Object.assign(f.context, { decodeAudioData: decode });
    const fetcher = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => new Response(new Uint8Array([1, 2, 3]), {
      headers: responseHeaders(chosen, chosen.provider),
    }));
    setTestGlobal('fetch', fetcher);
    const plan = await prepareAnnouncement({ text: '', amountVnd: '1000', transactionId: 'tx' }, chosen,
      new AbortController().signal, f.options);
    expect(plan.kind).toBe('buffers');
    expect(decode).toHaveBeenCalledTimes(1);
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(JSON.parse(fetcher.mock.calls[0][1]!.body as string)).toEqual({ profileRevision: 1, part: 'amount' });
    const playing = playPrepared(plan, new AbortController().signal, f.options);
    expect(f.sources[0].playbackRate.value).toBe(1);
    f.sources[0].onended!(); await playing;
    plan.release(); f.options.assets.dispose(); f.options.browser.dispose();
  });

  it.each([
    { 'X-TTS-Provider': 'edge' }, { 'X-TTS-Voice': 'vi-VN-HoaiMyNeural' },
    { 'X-Voice-Revision': '2' }, { 'X-Voice-Synthesis-Key': 'b'.repeat(64) },
  ])('rejects mismatched Piper metadata before decode without another request: %j', async (override) => {
    setTestGlobal('window', {});
    const f = fixture(), decode = vi.fn();
    Object.assign(f.context, { decodeAudioData: decode });
    vi.spyOn(f.options.assets, 'getManifest').mockResolvedValue(null);
    const fetcher = vi.fn(async () => new Response(new Uint8Array([1]), { headers: { ...responseHeaders(), ...override } }));
    setTestGlobal('fetch', fetcher);
    await expect(prepareAnnouncement({ text: '', amountVnd: '1000', transactionId: 'tx' }, profile,
      new AbortController().signal, f.options)).rejects.toThrow('VOICE_AUDIO_PROFILE_MISMATCH');
    expect(fetcher).toHaveBeenCalledTimes(1); expect(decode).not.toHaveBeenCalled();
    f.options.assets.dispose(); f.options.browser.dispose();
  });

  it.each(['amount', 'classic'] as const)('uses an exact-profile Piper %s clip without cold synthesis', async (kind) => {
    const f = fixture(), chosen = { ...profile, funEnabled: kind === 'classic' };
    vi.spyOn(catalogModule, 'loadVoiceCatalog').mockResolvedValue(null);
    const classic = { ...recipe, id: 'classic-1', styleId: 'classic', fragmentIds: [],
      template: 'Đã nhận {amount}. Cảm ơn quý khách.' };
    const transcript = kind === 'classic' ? 'Đã nhận một nghìn đồng. Cảm ơn quý khách.' : 'Đã nhận một nghìn đồng.';
    const clip = { clipId: 'exact', kind, transcript, durationMs: 1000, sampleRate: 24000, bytes: 3, sha256: 'c'.repeat(64) };
    vi.spyOn(f.options.assets, 'getManifest').mockResolvedValue({ schemaVersion: 2, packVersion: 'd'.repeat(64),
      preloadClipIds: [], composition: { tail: 'whole', numbers: 'whole' }, qualityEvidenceSha256: 'e'.repeat(64), numberSegments: {},
      synthesisKey: chosen.synthesisKey, voiceId: chosen.voiceId, rate: chosen.rate, pitchHz: 0,
      formatterVersion: 'money-vi-v1', catalogVersion: 'v1', clips: [clip],
      amounts: { '1000': { standard: 'exact', classic: { 'classic-1': 'exact' } } } });
    const getDecoded = vi.spyOn(f.options.assets, 'getDecoded').mockResolvedValue(decoded);
    const fetcher = vi.fn(); setTestGlobal('fetch', fetcher);
    const plan = await prepareAnnouncement({ text: '', amountVnd: '1000', transactionId: 'tx',
      ...(kind === 'classic' ? { recipe: classic, phraseId: classic.id } : {}) }, chosen,
      new AbortController().signal, f.options);
    expect(plan.kind).toBe('buffers'); expect(getDecoded).toHaveBeenCalledTimes(1);
    expect(fetcher).not.toHaveBeenCalled();
    plan.release(); f.options.assets.dispose(); f.options.browser.dispose();
  });
  it('keeps Piper warm fragment joining disabled and requests a single complete tail', async () => {
    setTestGlobal('window', {});
    const f = fixture(), chosen = { ...profile, providerMode: 'PIPER_ONLY' as const, voiceId: 'vi-VN-vais1000-medium', funEnabled: true,
      amountBands: profile.amountBands.map((band) => ({ ...band, styleId: 'cute' as const })) };
    Object.assign(f.context, { decodeAudioData: vi.fn(async () => decoded) });
    vi.spyOn(catalogModule, 'loadVoiceCatalog').mockResolvedValue(null);
    const core = { clipId: 'core', kind: 'amount' as const, transcript: 'Đã nhận một nghìn đồng.',
      durationMs: 1000, sampleRate: 24000, bytes: 3, sha256: 'c'.repeat(64) };
    const fragments = recipe.fragmentIds.map((clipId) => ({ ...core, clipId, kind: 'tail-fragment' as const }));
    vi.spyOn(f.options.assets, 'getManifest').mockResolvedValue({ schemaVersion: 2, packVersion: 'd'.repeat(64),
      preloadClipIds: [], composition: { tail: 'whole', numbers: 'whole' }, qualityEvidenceSha256: 'e'.repeat(64), numberSegments: {},
      synthesisKey: chosen.synthesisKey, voiceId: chosen.voiceId, rate: chosen.rate, pitchHz: 0,
      formatterVersion: 'money-vi-v1', catalogVersion: 'v1', clips: [core, ...fragments],
      amounts: { '1000': { standard: 'core', classic: {} } } });
    const getDecoded = vi.spyOn(f.options.assets, 'getDecoded').mockResolvedValue(decoded);
    const fetcher = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) =>
      new Response(new Uint8Array([1, 2, 3]), { headers: responseHeaders(chosen, 'piper') }));
    setTestGlobal('fetch', fetcher);
    const plan = await prepareAnnouncement({ text: '', amountVnd: '1000', transactionId: 'tx', recipe, phraseId: recipe.id },
      chosen, new AbortController().signal, f.options);
    expect(plan.kind).toBe('buffers');
    if (plan.kind !== 'buffers') throw new Error('Expected prepared amount buffer');
    expect(plan.segments).toHaveLength(2);
    expect(plan.segments[0].role).toBe('amount');
    expect(plan.segments[1].role).toBe('tail');
    expect(getDecoded).toHaveBeenCalledTimes(1);
    expect(getDecoded.mock.calls[0][0].clipId).toBe('core');
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(JSON.parse(fetcher.mock.calls[0][1]!.body as string)).toMatchObject({ part: 'tail', phraseId: recipe.id });
    plan.release(); f.options.assets.dispose(); f.options.browser.dispose();
  });
});
