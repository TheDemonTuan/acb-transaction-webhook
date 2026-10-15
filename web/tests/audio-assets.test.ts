import { afterEach, describe, expect, it, vi } from 'bun:test';
import { setTestGlobal, restoreTestGlobals } from './test-globals';
import { AudioAssets, sha256, validateManifest, type VoiceClip, type VoiceManifest } from '../src/features/voice-announcements/audio-assets';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';

const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64) };
const clip = async (index: number, size = 16): Promise<{ descriptor: VoiceClip; bytes: ArrayBuffer }> => {
  const bytes = new Uint8Array(size); bytes[0] = index;
  return { bytes: bytes.buffer, descriptor: { clipId: `amount-${index}`, kind: 'amount', transcript: 'Đã nhận một nghìn đồng.',
    durationMs: 1000, sampleRate: 24000, bytes: size, sha256: await sha256(bytes.buffer) } };
};
// The fake supplies only the DOM decoder interface exercised here; it is not a
// replacement for the upstream audio/listening acceptance gate.
const context = (length = 24000) => {
  const decodeAudioData = vi.fn(async () => ({ length, numberOfChannels: 1, duration: length / 24000 } as AudioBuffer));
  return Object.assign({ decodeAudioData } as unknown as AudioContext, { decodeAudioData });
};

describe('bounded browser audio assets', () => {
  afterEach(() => { restoreTestGlobals(); vi.restoreAllMocks(); });
  it('bounds compressed fallback to 8MiB and decoded entries to 32', async () => {
    setTestGlobal('caches', undefined);
    const values = await Promise.all(Array.from({ length: 40 }, (_, index) => clip(index)));
    const byHash = new Map(values.map((value) => [value.descriptor.sha256, value.bytes]));
    setTestGlobal('fetch', vi.fn(async (url: string) => new Response(byHash.get(url.split('/').at(-1)!))));
    const assets = new AudioAssets(true), decoder = context();
    const owner = {}, signal = new AbortController().signal;
    for (const value of values) { await assets.getDecoded(value.descriptor, profile, decoder, owner, signal); assets.release(owner); }
    expect(assets.getStats()).toMatchObject({ decodedCount: 32, cacheLimited: true });
    expect(assets.getStats().decodedBytes).toBeLessThanOrEqual(16 * 1024 * 1024);
    const large = await Promise.all(Array.from({ length: 10 }, (_, index) => clip(index + 40, 1024 * 1024)));
    for (const value of large) { byHash.set(value.descriptor.sha256, value.bytes); await assets.getCompressed(value.descriptor, signal); }
    expect(assets.getStats().compressedBytes).toBeLessThanOrEqual(8 * 1024 * 1024);
    assets.dispose();
    expect(assets.getStats()).toMatchObject({ decodedCount: 0, compressedBytes: 0 });
  });
  it('never exceeds decoded cap because entries are pinned; declines oversized decode', async () => {
    setTestGlobal('caches', undefined);
    const values = await Promise.all(Array.from({ length: 10 }, (_, index) => clip(index)));
    const byHash = new Map(values.map((value) => [value.descriptor.sha256, value.bytes]));
    setTestGlobal('fetch', vi.fn(async (url: string) => new Response(byHash.get(url.split('/').at(-1)!))));
    const assets = new AudioAssets(true), owner = {}, decoder = context(2 * 1024 * 1024 / 4);
    for (const value of values.slice(0, 8)) expect(await assets.getDecoded(value.descriptor, profile, decoder, owner, new AbortController().signal)).not.toBeNull();
    expect(await assets.getDecoded(values[8].descriptor, profile, decoder, {}, new AbortController().signal)).toBeNull();
    expect(assets.getStats().decodedBytes).toBe(16 * 1024 * 1024);
    assets.release(owner);
    expect(await assets.getDecoded(values[8].descriptor, profile, decoder, {}, new AbortController().signal)).not.toBeNull();
    const oversized = { ...values[9].descriptor, durationMs: 200000 };
    const calls = decoder.decodeAudioData.mock.calls.length;
    expect(await assets.getDecoded(oversized, profile, decoder, {}, new AbortController().signal)).toBeNull();
    expect(decoder.decodeAudioData.mock.calls).toHaveLength(calls);
    assets.dispose();
  });
  it('verifies checksum before decode and never retries an untrusted network response', async () => {
    setTestGlobal('caches', undefined);
    const value = await clip(1);
    const fetcher = vi.fn(async () => new Response(new Uint8Array(value.descriptor.bytes)));
    setTestGlobal('fetch', fetcher);
    const assets = new AudioAssets(true), decoder = context();
    await expect(assets.getDecoded(value.descriptor, profile, decoder, {}, new AbortController().signal)).rejects.toThrow('VOICE_ASSET_CORRUPT');
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(decoder.decodeAudioData).not.toHaveBeenCalled();
    assets.dispose();
  });
  it('does not insert a decoded buffer after cancel, and clears old unpinned synthesis keys', async () => {
    setTestGlobal('caches', undefined);
    const value = await clip(1);
    setTestGlobal('fetch', vi.fn(async () => new Response(value.bytes)));
    const { promise, resolve } = Promise.withResolvers<AudioBuffer>();
    const { promise: started, resolve: decoding } = Promise.withResolvers<void>();
    const decoder = { decodeAudioData: () => { decoding(); return promise; } } as unknown as AudioContext;
    const assets = new AudioAssets(true), abort = new AbortController();
    const pending = assets.getDecoded(value.descriptor, profile, decoder, {}, abort.signal);
    await started; abort.abort(); resolve({ length: 24000, numberOfChannels: 1, duration: 1 } as AudioBuffer);
    await expect(pending).rejects.toThrow('VOICE_CANCELLED');
    expect(assets.getStats().decodedCount).toBe(0);
    const owner = {};
    await assets.getDecoded(value.descriptor, profile, context(), owner, new AbortController().signal);
    assets.changeProfile('b'.repeat(64));
    expect(assets.getStats().decodedCount).toBe(1);
    assets.release(owner); assets.changeProfile('b'.repeat(64));
    expect(assets.getStats().decodedCount).toBe(0);
    assets.dispose();
  });
  it('rejects packs for another synthesis profile before any clip download', async () => {
    const manifest: VoiceManifest = { schemaVersion: 2, packVersion: 'c'.repeat(64), synthesisKey: 'b'.repeat(64),
      voiceId: profile.voiceId, rate: profile.rate, pitchHz: 0, formatterVersion: 'money-vi-v1', catalogVersion: 'v1',
      qualityEvidenceSha256: 'e'.repeat(64), preloadClipIds: [], composition: { tail: 'whole', numbers: 'whole' }, clips: [], amounts: {} };
    await expect(validateManifest(manifest, profile)).rejects.toThrow('VOICE_PACK_MISMATCH');
  });
  it('preloads only six active-profile cores plus eight fragments through real verified fetch/decode, concurrency two', async () => {
    setTestGlobal('caches', undefined);
    const assets = new AudioAssets(true);
    const amounts = ['20000', '50000', '100000', '200000', '500000', '1000000', '5000000'];
    const inputs = await Promise.all(Array.from({ length: 27 }, (_, index) => clip(index + 1)));
    const clips = inputs.map((value, index): VoiceClip => ({ ...value.descriptor,
      clipId: index < amounts.length ? `amount-${amounts[index]}` : `fragment-${index - amounts.length}`,
      kind: index < amounts.length ? 'amount' : 'tail-fragment',
    }));
    const preloadClipIds = [...amounts.slice(0, 6).map((a) => `amount-${a}`), ...Array.from({ length: 8 }, (_, i) => `fragment-${i}`)];
    const unsigned = { schemaVersion: 2, synthesisKey: profile.synthesisKey, voiceId: profile.voiceId,
      rate: profile.rate, pitchHz: 0, formatterVersion: 'money-vi-v1', catalogVersion: 'v1',
      qualityEvidenceSha256: 'e'.repeat(64), preloadClipIds,
      composition: { tail: 'whole', numbers: 'whole' }, clips,
      amounts: Object.fromEntries(amounts.map((amount) => [amount, { standard: `amount-${amount}`, classic: {} }])) };
    const canonical = (value: unknown): string => {
      if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
      if (value && typeof value === 'object') {
        const record = value as Record<string, unknown>;
        return `{${Object.keys(record).sort().map((key) => `${JSON.stringify(key)}:${canonical(record[key])}`).join(',')}}`;
      }
      return JSON.stringify(value);
    };
    const manifest = { ...unsigned, packVersion: await sha256(new TextEncoder().encode(canonical(unsigned)).buffer) };
    const inputByHash = new Map(inputs.map((value) => [value.descriptor.sha256, value.bytes]));
    const descriptorByHash = new Map(clips.map((value) => [value.sha256, value]));
    let active = 0, maximum = 0;
    const requested: string[] = [];
    setTestGlobal('fetch', vi.fn(async (url: string) => {
      if (url.endsWith('/manifest')) return new Response(JSON.stringify(manifest));
      const hash = url.split('/').at(-1)!;
      requested.push(descriptorByHash.get(hash)!.clipId);
      active++; maximum = Math.max(maximum, active);
      await Promise.resolve(); active--;
      return new Response(inputByHash.get(hash));
    }));
    const decoder = context();
    await assets.preload(profile, decoder);
    expect(requested).toEqual(expect.arrayContaining(amounts.slice(0, 6).map((amount) => `amount-${amount}`)));
    expect(requested).toHaveLength(14);
    expect(requested).not.toContain('amount-5000000');
    expect(requested).not.toContain('fragment-8');
    expect(maximum).toBe(2);
    expect(decoder.decodeAudioData).toHaveBeenCalledTimes(14);
    expect(assets.getStats().decodedCount).toBe(14);

    const cancelled = new AudioAssets(true);
    const requests: AbortSignal[] = [];
    const started = Promise.withResolvers<void>();
    setTestGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith('/manifest')) return new Response(JSON.stringify(manifest));
      const signal = init.signal!;
      requests.push(signal);
      if (requests.length === 2) started.resolve();
      const { promise, reject } = Promise.withResolvers<Response>();
      signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
      return promise;
    }));
    const cancelDecoder = context();
    const preloading = cancelled.preload(profile, cancelDecoder);
    await started.promise;
    cancelled.changeProfile('b'.repeat(64));
    await expect(preloading).rejects.toThrow();
    expect(requests).toHaveLength(2);
    expect(requests.every((signal) => signal.aborted)).toBe(true);
    expect(cancelDecoder.decodeAudioData).not.toHaveBeenCalled();
    expect(cancelled.getStats().decodedCount).toBe(0);

    const failed = new AudioAssets(true), inFlight: AbortSignal[] = [];
    const bothStarted = Promise.withResolvers<void>();
    const stopSecond = Promise.withResolvers<void>();
    const siblingAborted = Promise.withResolvers<void>();
    setTestGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith('/manifest')) return new Response(JSON.stringify(manifest));
      const signal = init.signal!;
      inFlight.push(signal);
      if (inFlight.length === 1) {
        await bothStarted.promise;
        throw new Error('ASSET_NETWORK_FAILURE');
      }
      bothStarted.resolve();
      const { promise, reject } = Promise.withResolvers<Response>();
      signal.addEventListener('abort', () => {
        siblingAborted.resolve();
        void stopSecond.promise.then(() => reject(new DOMException('Aborted', 'AbortError')));
      }, { once: true });
      return promise;
    }));
    let settled = false;
    const failing = failed.preload(profile, context());
    const observed = failing.catch((error: unknown) => { settled = true; return error; });
    await bothStarted.promise;
    await siblingAborted.promise;
    expect(inFlight).toHaveLength(2);
    expect(settled).toBe(false);
    stopSecond.resolve();
    const error = await observed;
    expect(error).toMatchObject({ message: 'ASSET_NETWORK_FAILURE' });
    expect(inFlight.every((signal) => signal.aborted)).toBe(true);
    expect(inFlight).toHaveLength(2);
    failed.dispose();
    cancelled.dispose(); assets.dispose();
  });
});
