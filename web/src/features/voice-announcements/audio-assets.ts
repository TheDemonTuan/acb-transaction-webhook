import type { VoiceProfile } from './voice-profile';
import { readBounded, throwIfAborted } from './voice-catalog';

const MiB = 1024 * 1024;
const CACHE_NAME = 'acb.voice.clips.v1';
const MAX_AGE = 7 * 24 * 60 * 60 * 1000;
export interface VoiceClip {
  clipId: string;
  kind: 'amount' | 'classic' | 'tail-fragment' | 'tail-full' | 'amount-segment' | 'amount-prefix' | 'amount-suffix';
  transcript: string;
  durationMs: number;
  sampleRate: number;
  bytes: number;
  sha256: string;
  speechStartMs?: number | null;
  amountStartMs?: number | null;
}
export interface VoiceManifest {
  schemaVersion: 2;
  packVersion: string;
  synthesisKey: string;
  voiceId: string;
  rate: number;
  pitchHz: number;
  formatterVersion: 'money-vi-v1';
  catalogVersion: 'v1';
  qualityEvidenceSha256: string;
  preloadClipIds: string[];
  composition: {
    tail: 'whole' | 'fragments';
    numbers: 'whole' | 'segments';
  };
  numberSegments?: Record<string, string>;
  numberPrefix?: string | null;
  numberSuffix?: string | null;
  clips: VoiceClip[];
  amounts: Record<string, { standard: string; classic: Record<string, string> }>;
}

export async function sha256(data: ArrayBuffer): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', data);
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, '0')).join('');
}
function canonicalJSON(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(',')}]`;
  if (value && typeof value === 'object') {
    const record = value as Record<string, unknown>;
    return `{${Object.keys(record).sort().map((key) => `${JSON.stringify(key)}:${canonicalJSON(record[key])}`).join(',')}}`;
  }
  return JSON.stringify(value);
}
export async function validateManifest(data: unknown, profile: Readonly<VoiceProfile>): Promise<VoiceManifest> {
  const m = data as VoiceManifest;
  if (!m || m.schemaVersion !== 2 || !/^[a-f0-9]{64}$/.test(m.packVersion) || !/^[a-f0-9]{64}$/.test(m.qualityEvidenceSha256) ||
      m.synthesisKey !== profile.synthesisKey || m.voiceId !== profile.voiceId || m.rate !== profile.rate ||
      m.pitchHz !== profile.pitchHz || m.formatterVersion !== 'money-vi-v1' || m.catalogVersion !== profile.catalogVersion ||
      !Array.isArray(m.clips) || !m.amounts || typeof m.amounts !== 'object' || !Array.isArray(m.preloadClipIds)) throw new Error('VOICE_PACK_MISMATCH');
  const { packVersion: _version, ...unsigned } = m;
  if (await sha256(new TextEncoder().encode(canonicalJSON(unsigned)).buffer) !== m.packVersion) throw new Error('VOICE_PACK_CORRUPT');
  const ids = new Set<string>();
  let total = 0;
  for (const clip of m.clips) {
    if (!clip || typeof clip.clipId !== 'string' || ids.has(clip.clipId) || !/^[a-f0-9]{64}$/.test(clip.sha256) ||
        !Number.isSafeInteger(clip.bytes) || clip.bytes <= 0 || clip.bytes > 4 * MiB || clip.sampleRate !== 24000 ||
        !Number.isFinite(clip.durationMs) || clip.durationMs <= 0 || typeof clip.transcript !== 'string' ||
        !['amount', 'classic', 'tail-fragment', 'tail-full', 'amount-segment', 'amount-prefix', 'amount-suffix'].includes(clip.kind)) throw new Error('VOICE_PACK_CORRUPT');
    ids.add(clip.clipId); total += clip.bytes;
  }
  if (total > 64 * MiB) throw new Error('VOICE_PACK_TOO_LARGE');
  for (const [amount, mapping] of Object.entries(m.amounts)) {
    if (!/^[1-9][0-9]*$/.test(amount) || BigInt(amount) > 9007199254740991n || !mapping || !ids.has(mapping.standard) ||
        !mapping.classic || Object.values(mapping.classic).some((id) => !ids.has(id))) throw new Error('VOICE_PACK_CORRUPT');
  }
  return m;
}

interface DecodedEntry { buffer: AudioBuffer; bytes: number; pins: Set<object>; key: string }
interface CompressedEntry { bytes: ArrayBuffer; touched: number }

// One owner serializes persistent LRU changes. Decoded entries can be pinned only by
// current and next plans; the cap is checked before insertion, including pinned bytes.
export class AudioAssets {
  public cacheLimited = false;
  private compressed = new Map<string, CompressedEntry>();
  private compressedBytes = 0;
  private decoded = new Map<string, DecodedEntry>();
  private decodedBytes = 0;
  private persistent: Promise<Cache | null> | null = null;
  private serial: Promise<unknown> = Promise.resolve();
  private manifest: VoiceManifest | null = null;
  private manifestKey = '';
  private disposed = false;
  private preloadController: AbortController | null = null;
  constructor(private readonly isPublic: boolean) {}

  public getStats() {
    return { compressedBytes: this.compressedBytes, compressedCount: this.compressed.size, decodedBytes: this.decodedBytes,
      decodedCount: this.decoded.size, cacheLimited: this.cacheLimited };
  }

  private async cache(): Promise<Cache | null> {
    if (!this.persistent) {
      this.persistent = (async () => {
        try {
          if (typeof caches === 'undefined') throw new Error('CACHE_UNAVAILABLE');
          return await caches.open(CACHE_NAME);
        } catch { this.cacheLimited = true; return null; }
      })();
    }
    return this.persistent;
  }

  public async getManifest(profile: Readonly<VoiceProfile>, signal: AbortSignal): Promise<VoiceManifest | null> {
    throwIfAborted(signal);
    if (this.disposed) throw new Error('VOICE_DISPOSED');
    if (this.manifestKey === profile.synthesisKey && this.manifest) return this.manifest;
    if (!/^[a-f0-9]{64}$/.test(profile.synthesisKey)) return null;
    try {
      const response = await fetch(`${this.isPublic ? '/api/public/v1' : '/api/v1'}/voice/profiles/${profile.synthesisKey}/manifest`, {
        signal, credentials: 'same-origin', cache: 'no-cache',
      });
      if (!response.ok) return null;
      const bytes = await readBounded(response, MiB, signal);
      const manifest = await validateManifest(JSON.parse(new TextDecoder().decode(bytes)), profile);
      throwIfAborted(signal);
      this.manifestKey = profile.synthesisKey;
      this.manifest = manifest;
      return manifest;
    } catch (error) { throwIfAborted(signal); return null; }
  }

  private remember(hash: string, bytes: ArrayBuffer): void {
    const existing = this.compressed.get(hash);
    if (existing) { this.compressed.delete(hash); this.compressedBytes -= existing.bytes.byteLength; }
    while (this.compressedBytes + bytes.byteLength > 8 * MiB || this.compressed.size >= 512) {
      const oldest = this.compressed.entries().next().value;
      if (!oldest) break;
      this.compressed.delete(oldest[0]); this.compressedBytes -= oldest[1].bytes.byteLength;
    }
    this.compressed.set(hash, { bytes, touched: Date.now() }); this.compressedBytes += bytes.byteLength;
  }

  private async persistentRead(url: string, signal: AbortSignal): Promise<ArrayBuffer | null> {
    const cache = await this.cache();
    if (!cache) return null;
    try {
      const response = await cache.match(url);
      if (!response) return null;
      if (Date.now() - Number(response.headers.get('X-Voice-Stored-At') || 0) > MAX_AGE) {
        await cache.delete(url); return null;
      }
      return await readBounded(response, 4 * MiB, signal);
    } catch { throwIfAborted(signal); this.cacheLimited = true; return null; }
  }

  private async store(url: string, bytes: ArrayBuffer, signal: AbortSignal): Promise<void> {
    const task = this.serial.then(async () => {
      throwIfAborted(signal);
      const cache = await this.cache();
      if (!cache || this.disposed) return;
      try {
        const keys = await cache.keys();
        const entries: { request: Request; size: number; touched: number; stored: number }[] = [];
        let total = 0;
        for (const request of keys) {
          const entry = await cache.match(request);
          const size = Number(entry?.headers.get('Content-Length') || 0);
          const stored = Number(entry?.headers.get('X-Voice-Stored-At') || 0);
          if (request.url === new URL(url, location.href).href || Date.now() - stored > MAX_AGE || size <= 0) {
            await cache.delete(request); continue;
          }
          entries.push({ request, size, stored, touched: Number(entry?.headers.get('X-Voice-Touched-At') || stored) }); total += size;
        }
        entries.sort((a, b) => a.touched - b.touched);
        while (total + bytes.byteLength > 64 * MiB || entries.length >= 512) {
          const oldest = entries.shift();
          if (!oldest) break;
          await cache.delete(oldest.request); total -= oldest.size;
        }
        throwIfAborted(signal);
        await cache.put(url, new Response(bytes, { headers: {
          'Content-Type': 'audio/mpeg', 'Content-Length': String(bytes.byteLength),
          'X-Voice-Stored-At': String(Date.now()), 'X-Voice-Touched-At': String(Date.now()),
        } }));
      } catch { throwIfAborted(signal); this.cacheLimited = true; }
    });
    this.serial = task.catch(() => {});
    await task;
  }

  public async getCompressed(clip: VoiceClip, signal: AbortSignal): Promise<ArrayBuffer> {
    throwIfAborted(signal);
    if (this.disposed) throw new Error('VOICE_DISPOSED');
    const url = `${this.isPublic ? '/api/public/v1' : '/api/v1'}/voice/assets/${clip.sha256}`;
    const memory = this.compressed.get(clip.sha256);
    let bytes: ArrayBuffer | null = null;
    if (memory && Date.now() - memory.touched <= MAX_AGE) bytes = memory.bytes;
    else bytes = await this.persistentRead(url, signal);
    if (bytes && (bytes.byteLength !== clip.bytes || await sha256(bytes) !== clip.sha256)) {
      this.compressed.delete(clip.sha256);
      if (memory) this.compressedBytes -= memory.bytes.byteLength;
      bytes = null;
      try { await (await this.cache())?.delete(url); } catch { this.cacheLimited = true; }
    }
    // A corrupt local entry gets exactly one verified network fetch, never synthesis retry.
    if (!bytes) {
      const response = await fetch(url, { signal, credentials: 'same-origin', cache: 'reload' });
      if (!response.ok) throw new Error('VOICE_ASSET_UNAVAILABLE');
      bytes = await readBounded(response, 4 * MiB, signal);
      if (bytes.byteLength !== clip.bytes || await sha256(bytes) !== clip.sha256) throw new Error('VOICE_ASSET_CORRUPT');
    }
    throwIfAborted(signal);
    this.remember(clip.sha256, bytes);
    await this.store(url, bytes, signal);
    return bytes;
  }

  public async getDecoded(clip: VoiceClip, profile: Readonly<VoiceProfile>, context: AudioContext,
      owner: object, signal: AbortSignal): Promise<AudioBuffer | null> {
    throwIfAborted(signal);
    if (this.disposed) throw new Error('VOICE_DISPOSED');
    const cached = this.decoded.get(clip.sha256);
    if (cached && cached.key === profile.synthesisKey) {
      cached.pins.add(owner); this.decoded.delete(clip.sha256); this.decoded.set(clip.sha256, cached);
      return cached.buffer;
    }
    const bytes = await this.getCompressed(clip, signal);
    throwIfAborted(signal);
    // Estimate PCM first; oversized clips stay compressed and use native playback.
    if (Math.ceil(clip.durationMs / 1000 * clip.sampleRate) * 4 > 16 * MiB) return null;
    const buffer = await context.decodeAudioData(bytes.slice(0));
    throwIfAborted(signal);
    return this.retainDecoded(clip.sha256, buffer, profile.synthesisKey, owner);
  }

  public retainDecoded(id: string, buffer: AudioBuffer, key: string, owner: object): AudioBuffer | null {
    if (this.disposed) throw new Error('VOICE_DISPOSED');
    const size = buffer.length * buffer.numberOfChannels * 4;
    if (size > 16 * MiB) return null;
    const raced = this.decoded.get(id);
    if (raced && raced.key === key) { raced.pins.add(owner); return raced.buffer; }
    if (raced) { this.decoded.delete(id); this.decodedBytes -= raced.bytes; }
    for (const [hash, entry] of this.decoded) {
      if (this.decodedBytes + size <= 16 * MiB && this.decoded.size < 32) break;
      if (entry.pins.size === 0) { this.decoded.delete(hash); this.decodedBytes -= entry.bytes; }
    }
    if (this.decodedBytes + size > 16 * MiB || this.decoded.size >= 32) return null;
    this.decoded.set(id, { buffer, bytes: size, pins: new Set([owner]), key });
    this.decodedBytes += size;
    return buffer;
  }

  public release(owner: object): void {
    for (const entry of this.decoded.values()) entry.pins.delete(owner);
  }

  public cancelPreload(): void {
    this.preloadController?.abort();
    this.preloadController = null;
  }

  public changeProfile(key: string): void {
    this.preloadController?.abort();
    this.preloadController = null;
    if (this.manifestKey !== key) { this.manifest = null; this.manifestKey = ''; }
    for (const [hash, entry] of this.decoded) {
      if (entry.key !== key && entry.pins.size === 0) { this.decoded.delete(hash); this.decodedBytes -= entry.bytes; }
    }
  }

  public async preload(profile: Readonly<VoiceProfile>, context: AudioContext, signal?: AbortSignal): Promise<void> {
    this.preloadController?.abort();
    const controller = new AbortController();
    this.preloadController = controller;
    const abort = () => controller.abort();
    signal?.addEventListener('abort', abort, { once: true });
    if (signal?.aborted) controller.abort();
    const owner = {};
    try {
      const manifest = await this.getManifest(profile, controller.signal);
      if (!manifest) return;
      const ids: string[] = (Array.isArray(manifest.preloadClipIds) && manifest.preloadClipIds.length > 0)
        ? manifest.preloadClipIds
        : [
            ...['20000', '50000', '100000', '200000', '500000', '1000000'].map((a) => manifest.amounts[a]?.standard).filter(Boolean),
            ...manifest.clips.filter((c) => c.kind === 'tail-fragment').slice(0, 8).map((c) => c.clipId),
          ];
      let index = 0;
      const worker = async () => {
        while (index < ids.length) {
          throwIfAborted(controller.signal);
          const id = ids[index++];
          const clip = manifest.clips.find((entry) => entry.clipId === id);
          if (clip) await this.getDecoded(clip, profile, context, owner, controller.signal);
          this.release(owner);
        }
      };
      const workers = [worker(), worker()];
      try {
        await Promise.all(workers);
      } catch (error) {
        controller.abort();
        await Promise.allSettled(workers);
        throw error;
      }
    } finally {
      this.release(owner);
      signal?.removeEventListener('abort', abort);
      if (this.preloadController === controller) this.preloadController = null;
    }
  }

  public dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.preloadController?.abort();
    this.compressed.clear(); this.decoded.clear();
    this.compressedBytes = 0; this.decodedBytes = 0;
    this.manifest = null;
  }
}
