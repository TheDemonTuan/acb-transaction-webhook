import { afterEach, describe, expect, it, vi } from 'bun:test';
import { setTestGlobal, restoreTestGlobals } from './test-globals';
import { TestAudioContext, TestNativeAudio, resetAudioTestFixture } from './audio-test-fixture';
import { prepareAnnouncement, playPrepared, type PreparedAnnouncement, type PreparationOptions } from '../src/features/voice-announcements/announcement-plan';
import { AudioAssets } from '../src/features/voice-announcements/audio-assets';
import { RecentPhraseRing } from '../src/features/voice-announcements/voice-copy';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import * as catalogModule from '../src/features/voice-announcements/voice-catalog';

function fixture() {
  resetAudioTestFixture();
  const context = new TestAudioContext();
  const options: PreparationOptions = { isPublic: true, context: context as unknown as AudioContext,
    assets: new AudioAssets(true), recent: new RecentPhraseRing() };
  return { context, options, sources: context.sources };
}
const buffer = (duration: number) => ({ duration }) as AudioBuffer;
const recipe = { id: 'cute-small-o001-c001', styleId: 'cute', bandId: 'small', weight: 1,
  openingId: 'o', closingId: 'c', tail: 'Cảm ơn quý khách.', fragmentIds: ['o', 'c'] };
const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64), funEnabled: false };
const decoded = { duration: 1, length: 24000, numberOfChannels: 1 } as AudioBuffer;
const responseHeaders = (p = profile) => ({
  'Content-Type': 'audio/mpeg', 'X-TTS-Provider': 'vieneu', 'X-TTS-Voice': p.voiceId, 'X-TTS-Fallback': 'false',
  'X-Voice-Revision': String(p.revision), 'X-Voice-Synthesis-Key': p.synthesisKey,
});

function expectSpeechGraph(context: TestAudioContext, input: { connect: { mock: { calls: unknown[][] } } }, volume: number) {
  const boost = context.gains.find((gain) => gain.gain.value === 2)!;
  const local = context.gains.find((gain) => gain !== boost && gain.gain.value === volume)!;
  expect(boost).toBeDefined(); expect(local).toBeDefined();
  expect(input.connect.mock.calls).toContainEqual([boost]);
  expect(context.shapers).toHaveLength(1);
  const shaper = context.shapers[0];
  expect(boost.connect).toHaveBeenCalledWith(shaper);
  expect(shaper.connect).toHaveBeenCalledWith(local);
  expect(local.connect).toHaveBeenCalledWith(context.destination);
  expect(shaper.curve).toBeInstanceOf(Float32Array);
  expect(shaper.curve!.length).toBeGreaterThan(1);
  const curve = Array.from(shaper.curve!);
  expect(curve.every((sample) => Number.isFinite(sample) && Math.abs(sample) <= 10 ** (-1 / 20))).toBe(true);
  expect(curve.every((sample, index) => index === 0 || sample >= curve[index - 1])).toBe(true);
  expect(curve[Math.floor(curve.length / 2)]).toBe(0);
  expect(curve[0]).toBeCloseTo(-curve.at(-1)!);
  expect(curve[0]).toBeLessThan(-0.75); expect(curve.at(-1)).toBeGreaterThan(0.75);
}

describe('prepared audio timeline', () => {
  afterEach(() => { vi.useRealTimers(); restoreTestGlobals(); vi.restoreAllMocks(); });
  it('schedules complete core and approved tail buffers on one audio clock after chime', async () => {
    vi.useFakeTimers();
    const f = fixture(), onTailStart = vi.fn(), onStart = vi.fn();
    const plan: PreparedAnnouncement = { kind: 'buffers', profile, volume: 0.4, recipe,
      segments: [{ buffer: buffer(2), role: 'amount' }, { buffer: buffer(1), role: 'tail' }, { buffer: buffer(0.5), role: 'tail' }],
      message: { text: '', onStart, onTailStart }, release: vi.fn() };
    const pending = playPrepared(plan, new AbortController().signal, f.options, 10.85);
    expect(f.sources.map((source) => source.start.mock.calls[0][0])).toEqual([10.85, 12.85, 13.85]);
    expect(f.sources.every((source) => source.playbackRate.value === 1)).toBe(true);
    expectSpeechGraph(f.context, f.sources[0], 0.4);
    vi.advanceTimersByTime(851);
    expect(onStart).toHaveBeenCalledTimes(1); expect(onTailStart).not.toHaveBeenCalled();
    vi.advanceTimersByTime(2000);
    expect(onTailStart).toHaveBeenCalledWith(recipe.id);
    f.sources.at(-1)!.onended!(); await pending;
    expect(f.sources.every((source) => source.disconnect.mock.calls.length === 1)).toBe(true);
    expect(f.context.shapers[0].disconnect).toHaveBeenCalledTimes(1);
    f.options.assets.dispose();
  });
  it('cancel stops all nodes and settles once without late start or tail callbacks', async () => {
    vi.useFakeTimers();
    const f = fixture(), onStart = vi.fn(), onTailStart = vi.fn(), controller = new AbortController();
    const plan: PreparedAnnouncement = { kind: 'buffers', profile, volume: 1, recipe,
      segments: [{ buffer: buffer(2), role: 'amount' }, { buffer: buffer(1), role: 'tail' }], message: { text: '', onStart, onTailStart }, release: vi.fn() };
    const pending = playPrepared(plan, controller.signal, f.options, 10.85);
    controller.abort();
    await expect(pending).rejects.toMatchObject({ message: 'VOICE_CANCELLED', playbackStarted: false });
    vi.advanceTimersByTime(5000);
    expect(onStart).not.toHaveBeenCalled(); expect(onTailStart).not.toHaveBeenCalled();
    expect(f.sources.every((source) => source.stop.mock.calls.length === 1)).toBe(true);
    f.options.assets.dispose();
  });
  it.each([false, true])('stops scheduled buffers on statechange with playbackStarted=%s only after onStart', async (started) => {
    vi.useFakeTimers();
    const f = fixture(), onStart = vi.fn(), onTailStart = vi.fn();
    const plan: PreparedAnnouncement = { kind: 'buffers', profile, volume: 1, recipe,
      segments: [{ buffer: buffer(2), role: 'amount' }, { buffer: buffer(1), role: 'tail' }], message: { text: '', onStart, onTailStart }, release: vi.fn() };
    const pending = playPrepared(plan, new AbortController().signal, f.options, 10.85);
    if (started) vi.advanceTimersByTime(851);
    f.context.setState('suspended');
    await expect(pending).rejects.toMatchObject({ message: 'AUDIO_CONTEXT_NOT_RUNNING', playbackStarted: started });
    vi.advanceTimersByTime(10000);
    expect(onStart).toHaveBeenCalledTimes(started ? 1 : 0); expect(onTailStart).not.toHaveBeenCalled();
    expect(f.sources.every((source) => source.stop.mock.calls.length === 1)).toBe(true);
    f.options.assets.dispose();
  });
  it('routes media through the same gain/curve/local-volume graph without native volume attenuation', async () => {
    const f = fixture(), audio = new TestNativeAudio(), onStart = vi.fn();
    const plan: PreparedAnnouncement = { kind: 'stream', profile, volume: 0.4,
      streamUrl: 'blob:test', audioElement: audio as unknown as HTMLAudioElement, message: { text: '', onStart }, release: vi.fn() };
    await playPrepared(plan, new AbortController().signal, f.options);
    expect(f.context.mediaSources).toHaveLength(1);
    expect(f.context.mediaSources[0].audio).toBe(audio as unknown as HTMLAudioElement);
    expectSpeechGraph(f.context, f.context.mediaSources[0].node, 0.4);
    expect(audio.volume).toBe(1); expect(audio.playbackRate).toBe(1);
    expect(onStart).toHaveBeenCalledTimes(1);
    expect(f.context.mediaSources[0].node.disconnect).toHaveBeenCalledTimes(1);
    f.options.assets.dispose();
  });
  it('colliding seeds reserve distinct recipes and release on cancellation or preparation failure', async () => {
    setTestGlobal('window', {});
    const f = fixture(), chosen = { ...profile, funEnabled: true,
      amountBands: profile.amountBands.map((band) => ({ ...band, styleId: 'cute' as const })) };
    vi.spyOn(catalogModule, 'loadVoiceCatalog').mockResolvedValue({ version: 'v1', amountCore: 'Đã nhận {amount}.',
      fragments: [{ id: 'o', text: 'Niềm vui vừa ghé.', styleId: 'cute', bandId: 'small' },
        { id: 'c', text: 'Cảm ơn quý khách.', styleId: 'cute', bandId: 'small' }], classic: [],
      recipes: [{ ...recipe, id: 'first' }, { ...recipe, id: 'second' }] });
    setTestGlobal('fetch', vi.fn(async () => new Response(new Uint8Array([1, 2, 3]), { headers: responseHeaders(chosen) })));
    const firstAbort = new AbortController();
    const [first, next] = await Promise.all([
      prepareAnnouncement({ text: '', transactionId: 'same-seed', amountVnd: '1000' }, chosen, firstAbort.signal, f.options),
      prepareAnnouncement({ text: '', transactionId: 'same-seed', amountVnd: '1000' }, chosen, new AbortController().signal, f.options),
    ]);
    expect(first.recipe!.id).not.toBe(next.recipe!.id); expect(f.options.recent.snapshot()).toHaveLength(2);
    firstAbort.abort(); next.release(); expect(f.options.recent.snapshot()).toEqual([]);
    setTestGlobal('fetch', vi.fn(async () => { throw new Error('NETWORK_FAILURE'); }));
    await expect(prepareAnnouncement({ text: '', transactionId: 'same-seed', amountVnd: '1000' }, chosen,
      new AbortController().signal, f.options)).rejects.toThrow('NETWORK_FAILURE');
    expect(f.options.recent.snapshot()).toEqual([]); f.options.assets.dispose();
  });
});

describe('Vieneu audio preparation', () => {
  afterEach(() => { restoreTestGlobals(); vi.restoreAllMocks(); });
  it('verifies buffered metadata and leaves playback rate neutral', async () => {
    setTestGlobal('window', {});
    const f = fixture();
    vi.spyOn(f.options.assets, 'getManifest').mockResolvedValue(null);
    const fetcher = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => new Response(new Uint8Array([1, 2, 3]), { headers: responseHeaders() }));
    setTestGlobal('fetch', fetcher);
    const plan = await prepareAnnouncement({ text: '', amountVnd: '1000', transactionId: 'tx' }, profile,
      new AbortController().signal, f.options);
    expect(plan.kind).toBe('buffers'); expect(f.context.decodeAudioData).toHaveBeenCalledTimes(1);
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(JSON.parse(fetcher.mock.calls[0][1]!.body as string)).toEqual({ profileRevision: 1, part: 'amount' });
    const playing = playPrepared(plan, new AbortController().signal, f.options);
    expect(f.sources[0].playbackRate.value).toBe(1); f.sources[0].onended!(); await playing;
    plan.release(); f.options.assets.dispose();
  });
  it.each([
    { 'X-TTS-Provider': 'edge' }, { 'X-TTS-Voice': 'unapproved-voice' }, { 'X-TTS-Fallback': 'true' },
    { 'X-Voice-Revision': '2' }, { 'X-Voice-Synthesis-Key': 'b'.repeat(64) },
  ])('rejects mismatched Vieneu metadata before decode without another request: %j', async (override) => {
    setTestGlobal('window', {});
    const f = fixture(); vi.spyOn(f.options.assets, 'getManifest').mockResolvedValue(null);
    const fetcher = vi.fn(async () => new Response(new Uint8Array([1]), { headers: { ...responseHeaders(), ...override } }));
    setTestGlobal('fetch', fetcher);
    await expect(prepareAnnouncement({ text: '', amountVnd: '1000', transactionId: 'tx' }, profile,
      new AbortController().signal, f.options)).rejects.toThrow('VOICE_AUDIO_PROFILE_MISMATCH');
    expect(fetcher).toHaveBeenCalledTimes(1); expect(f.context.decodeAudioData).not.toHaveBeenCalled(); f.options.assets.dispose();
  });
  it.each(['amount', 'classic'] as const)('uses an exact-profile Vieneu %s clip without cold synthesis', async (kind) => {
    const f = fixture(), chosen = { ...profile, funEnabled: kind === 'classic' };
    vi.spyOn(catalogModule, 'loadVoiceCatalog').mockResolvedValue(null);
    const classic = { ...recipe, id: 'classic-1', styleId: 'classic', fragmentIds: [], template: 'Đã nhận {amount}. Cảm ơn quý khách.' };
    const transcript = kind === 'classic' ? 'Đã nhận một nghìn đồng. Cảm ơn quý khách.' : 'Đã nhận một nghìn đồng.';
    const clip = { clipId: 'exact', kind, transcript, durationMs: 1000, sampleRate: 24000, bytes: 3, sha256: 'c'.repeat(64) };
    vi.spyOn(f.options.assets, 'getManifest').mockResolvedValue({ schemaVersion: 2, packVersion: 'd'.repeat(64),
      preloadClipIds: [], composition: { tail: 'whole', numbers: 'whole' }, qualityEvidenceSha256: 'e'.repeat(64), numberSegments: {},
      synthesisKey: chosen.synthesisKey, voiceId: chosen.voiceId, rate: chosen.rate, pitchHz: 0,
      formatterVersion: 'money-vi-v1', catalogVersion: 'v1', clips: [clip], amounts: { '1000': { standard: 'exact', classic: { 'classic-1': 'exact' } } } });
    const getDecoded = vi.spyOn(f.options.assets, 'getDecoded').mockResolvedValue(decoded);
    const fetcher = vi.fn(); setTestGlobal('fetch', fetcher);
    const plan = await prepareAnnouncement({ text: '', amountVnd: '1000', transactionId: 'tx',
      ...(kind === 'classic' ? { recipe: classic, phraseId: classic.id } : {}) }, chosen, new AbortController().signal, f.options);
    expect(plan.kind).toBe('buffers'); expect(getDecoded).toHaveBeenCalledTimes(1); expect(fetcher).not.toHaveBeenCalled();
    plan.release(); f.options.assets.dispose();
  });
  it('requests one complete nonclassic phrase instead of joining warm fragments or a separate cold tail', async () => {
    setTestGlobal('window', {});
    const f = fixture(), chosen = { ...profile, funEnabled: true,
      amountBands: profile.amountBands.map((band) => ({ ...band, styleId: 'cute' as const })) };
    vi.spyOn(catalogModule, 'loadVoiceCatalog').mockResolvedValue(null);
    const manifest = vi.spyOn(f.options.assets, 'getManifest'), getDecoded = vi.spyOn(f.options.assets, 'getDecoded');
    const fetcher = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) =>
      new Response(new Uint8Array([1, 2, 3]), { headers: responseHeaders(chosen) }));
    setTestGlobal('fetch', fetcher);
    const plan = await prepareAnnouncement({ text: '', amountVnd: '1000', transactionId: 'tx', recipe, phraseId: recipe.id },
      chosen, new AbortController().signal, f.options);
    expect(plan.kind).toBe('buffers');
    if (plan.kind !== 'buffers') throw new Error('Expected complete phrase buffer');
    expect(plan.segments).toHaveLength(1); expect(plan.segments[0].role).toBe('full');
    expect(manifest).not.toHaveBeenCalled(); expect(getDecoded).not.toHaveBeenCalled(); expect(fetcher).toHaveBeenCalledTimes(1);
    expect(JSON.parse(fetcher.mock.calls[0][1]!.body as string)).toMatchObject({ part: 'full', phraseId: recipe.id });
    plan.release(); f.options.assets.dispose();
  });
});
