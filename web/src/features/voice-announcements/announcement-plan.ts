import { getCsrfToken, parseApiError } from '../../api';
import { type VoiceProfile, synthesisProvider } from './voice-profile';
import type { VoiceMessage, VoiceTelemetry } from './voice-engine';
import { AudioAssets } from './audio-assets';
import { loadVoiceCatalog, readBounded, throwIfAborted } from './voice-catalog';
import { formatAnnouncementTemplate, RecentPhraseRing, selectAnnouncementRecipe, type AnnouncementRecipe } from './voice-copy';
import { speakVnd } from './money-to-vietnamese';
import { createSpeechOutput } from './speech-output';
import { prepareStreamPlayback, type StreamPlayback } from './stream-playback';

interface PlanCommon {
  profile: Readonly<VoiceProfile>;
  recipe?: AnnouncementRecipe;
  amountVnd?: string;
  transactionId?: string;
  volume: number;
  message: VoiceMessage;
  release: () => void;
  audioPath?: VoiceTelemetry['audioPath'];
  amountOffsetMs?: number | null;
  amountEvidence?: VoiceTelemetry['amountEvidence'];
}
export interface PreparedSegment {
  buffer: AudioBuffer;
  role: 'full' | 'prefix' | 'amount' | 'currency' | 'tail';
  clipId?: string;
  speechStartMs?: number;
  amountStartMs?: number | null;
}

export type PreparedAnnouncement = PlanCommon & (
  | { kind: 'buffers'; segments: PreparedSegment[] }
  | { kind: 'stream'; streamUrl: string; audioElement: HTMLAudioElement; completed?: Promise<void> }
);
export interface PreparationOptions {
  isPublic: boolean;
  context: AudioContext | null;
  assets: AudioAssets;
  recent: RecentPhraseRing;
  onStatus?: (state: 'ready' | 'cold' | 'unavailable', detail?: string) => void;
}

export function voiceRoute(message: VoiceMessage, isPublic: boolean, profile: Readonly<VoiceProfile>, part: 'full' | 'amount' | 'tail') {
  const base = isPublic ? '/api/public/v1' : '/api/v1';
  let path: string;
  if (message.isTest) path = '/voice/test';
  else if (message.summaryTransactionIds && message.summaryTransactionIds.length >= 2) path = '/voice/transactions/summary';
  else if (message.transactionId) {
    path = `/voice/transactions/${encodeURIComponent(message.transactionId)}`;
    if (message.isReplay && !isPublic) path += '/replay';
  } else throw new Error('NO_ONLINE_ROUTE');
  const body = { profileRevision: profile.revision, phraseId: part === 'amount' ? undefined : message.phraseId,
    part, ...(message.summaryTransactionIds ? { transactionIds: message.summaryTransactionIds } : {}) };
  const query = new URLSearchParams({ profileRevision: String(profile.revision), part });
  if (body.phraseId) query.set('phraseId', body.phraseId);
  if (message.summaryTransactionIds) query.set('transactionIds', message.summaryTransactionIds.join(','));
  return { url: `${base}${path}`, streamUrl: `${base}${path}/stream?${query}`, body };
}

async function coldBuffer(message: VoiceMessage, profile: Readonly<VoiceProfile>, part: 'full' | 'amount' | 'tail',
    options: PreparationOptions, signal: AbortSignal, owner: object): Promise<AudioBuffer> {
  const route = voiceRoute(message, options.isPublic, profile, part);
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener('abort', abort, { once: true });
  if (signal.aborted) controller.abort();
  const timer = setTimeout(abort, profile.coldRequestTimeoutMs);
  try {
    const headers = new Headers({ 'Content-Type': 'application/json' });
    if (!options.isPublic) headers.set('X-CSRF-Token', await getCsrfToken());
    throwIfAborted(controller.signal);
    const response = await fetch(route.url, { method: 'POST', credentials: 'same-origin', signal: controller.signal,
      headers, body: JSON.stringify(route.body) });
    if (!response.ok) throw await parseApiError(response);
    if (response.headers.get('X-Voice-Revision') !== String(profile.revision) ||
        response.headers.get('X-Voice-Synthesis-Key') !== profile.synthesisKey ||
        response.headers.get('X-TTS-Voice') !== profile.voiceId ||
        response.headers.get('X-TTS-Provider') !== synthesisProvider(profile.providerMode) ||
        response.headers.get('X-TTS-Fallback') !== 'false') {
      await response.body?.cancel(); throw new Error('VOICE_AUDIO_PROFILE_MISMATCH');
    }
    const bytes = await readBounded(response, 4 * 1024 * 1024, controller.signal);
    throwIfAborted(signal);
    if (!options.context) throw new Error('AUDIO_CONTEXT_UNAVAILABLE');
    // The eight-second network budget does not truncate playback or local decode.
    clearTimeout(timer);
    const buffer = await options.context.decodeAudioData(bytes);
    throwIfAborted(signal);
    const retained = options.assets.retainDecoded(`cold:${profile.synthesisKey}:${profile.revision}:${message.transactionId || 'test'}:${message.phraseId || ''}:${part}`, buffer, profile.synthesisKey, owner);
    if (!retained) throw new Error('VOICE_DECODE_CAPACITY');
    return retained;
  } catch (error) {
    throwIfAborted(signal);
    if (controller.signal.aborted) throw new Error('TTS_TIMEOUT');
    throw error;
  } finally { clearTimeout(timer); signal.removeEventListener('abort', abort); }
}

async function prepareNative(url: string, signal: AbortSignal, timeout: number): Promise<HTMLAudioElement> {
  const AudioClass = typeof window !== 'undefined' ? window.Audio : undefined;
  if (!AudioClass) throw new Error('AUDIO_CONTEXT_UNAVAILABLE');
  const audio = new AudioClass();
  audio.preload = 'auto';
  return new Promise((resolve, reject) => {
    let settled = false;
    const clean = () => {
      clearTimeout(timer); audio.removeEventListener('canplay', ready); audio.removeEventListener('error', failed);
      signal.removeEventListener('abort', aborted);
    };
    const finish = (error?: Error) => {
      if (settled) return;
      settled = true; clean();
      if (error) { audio.pause(); audio.removeAttribute('src'); audio.load(); reject(error); }
      else resolve(audio);
    };
    const ready = () => finish();
    const failed = () => finish(new Error('AUDIO_PREPARATION_FAILED'));
    const aborted = () => finish(new Error('VOICE_CANCELLED'));
    const timer = setTimeout(() => finish(new Error('TTS_TIMEOUT')), timeout);
    audio.addEventListener('canplay', ready); audio.addEventListener('error', failed);
    signal.addEventListener('abort', aborted, { once: true });
    if (signal.aborted) { aborted(); return; }
    audio.src = url; audio.load();
  });
}

export async function prepareAnnouncement(message: VoiceMessage, profile: Readonly<VoiceProfile>, signal: AbortSignal,
    options: PreparationOptions): Promise<PreparedAnnouncement> {
  throwIfAborted(signal);
  const owner = {};
  let objectURL: string | undefined;
  let native: HTMLAudioElement | undefined;
  let transport: StreamPlayback | undefined;
  let released = false;
  const release = () => {
    if (released) return;
    released = true; options.assets.release(owner); options.recent.release(owner);
    transport?.release();
    if (native) { native.pause(); native.removeAttribute('src'); native.load(); }
    if (objectURL) URL.revokeObjectURL(objectURL);
    signal.removeEventListener('abort', release);
  };
  signal.addEventListener('abort', release, { once: true });
  try {
    const catalog = profile.funEnabled ? await loadVoiceCatalog(profile.catalogVersion, options.isPublic, signal) : null;
    throwIfAborted(signal);
    if (!message.recipe && message.amountVnd) {
      message.recipe = selectAnnouncementRecipe({ transactionId: message.transactionId || 'voice-test', amountVnd: message.amountVnd,
        profileRevision: profile.revision, catalogVersion: profile.catalogVersion, recentPhraseIds: options.recent.snapshot(), profile, catalog });
      message.phraseId = message.recipe?.id;
    }
    const amount = message.amountVnd;
    if (amount !== undefined && (!/^[1-9][0-9]*$/.test(amount) || BigInt(amount) > 9007199254740991n)) throw new Error('VOICE_INVALID_AMOUNT');
    const band = amount ? profile.amountBands.find((b) => BigInt(amount) >= BigInt(b.fromVnd) && (b.toVnd === null || BigInt(amount) < BigInt(b.toVnd))) : undefined;
    const custom = band?.styleId === 'custom' && profile.funEnabled;
    const amountOnly = !profile.funEnabled || (!message.recipe && !custom);
    if (!amountOnly && message.recipe) options.recent.reserve(owner, message.recipe.id);
    if (amount) {
      message.text = amountOnly ? `Đã nhận ${speakVnd(amount)}.` : custom
        ? formatAnnouncementTemplate(profile.customTemplate, { amount, description: message.description, includeDescription: profile.includeDescription })
        : message.recipe?.template
          ? formatAnnouncementTemplate(message.recipe.template, { amount, description: message.description, includeDescription: profile.includeDescription })
          : `Đã nhận ${speakVnd(amount)}. ${message.recipe?.tail || ''}`;
    }
    const common: PlanCommon = { profile, recipe: amountOnly ? undefined : message.recipe, amountVnd: amount,
      transactionId: message.transactionId, volume: Math.max(0, Math.min(1, message.volume ?? 1)), message, release };
    const context = options.context;
    const allowPackForVieneu = amountOnly || (!amountOnly && message.recipe?.styleId === 'classic');
    const manifest = !custom && !profile.includeDescription && amount && !message.summaryTransactionIds && allowPackForVieneu
      ? await options.assets.getManifest(profile, signal) : null;
    throwIfAborted(signal);
    const mapping = amount && manifest ? manifest.amounts[amount] : undefined;
    const classicId = !amountOnly && message.recipe?.styleId === 'classic' ? mapping?.classic[message.recipe.id] : undefined;
    const needsClassicFull = !amountOnly && message.recipe?.styleId === 'classic';
    const clipId = needsClassicFull ? classicId : mapping?.standard;
    const clip = manifest?.clips.find((entry) => entry.clipId === clipId);
    if (clip && amount && clip.transcript !== (classicId ? message.text : `Đã nhận ${speakVnd(amount)}.`)) throw new Error('VOICE_ASSET_TRANSCRIPT_MISMATCH');
    if (clip && context && (amountOnly || classicId)) {
      const buffer = await options.assets.getDecoded(clip, profile, context, owner, signal);
      if (buffer) {
        options.onStatus?.('ready', options.assets.cacheLimited ? 'Bộ nhớ âm thanh hạn chế' : undefined);
        const segments: PreparedSegment[] = [{
          buffer,
          role: 'amount',
          clipId: clip.clipId,
          speechStartMs: clip.speechStartMs ?? 0,
          amountStartMs: clip.amountStartMs ?? (clip.kind === 'amount' ? 400 : 0),
        }];
        return { ...common, kind: 'buffers', segments, audioPath: 'decoded-pack' };
      }
    }
    if (clip && (amountOnly || classicId) && typeof window !== 'undefined' && window.Audio) {
      const bytes = await options.assets.getCompressed(clip, signal);
      objectURL = URL.createObjectURL(new Blob([bytes], { type: 'audio/mpeg' }));
      native = await prepareNative(objectURL, signal, 8000);
      options.onStatus?.('ready');
      return { ...common, kind: 'stream', streamUrl: objectURL, audioElement: native, audioPath: 'compressed-pack' };
    }
    options.onStatus?.('cold', options.assets.cacheLimited ? 'Bộ nhớ âm thanh hạn chế · tổng hợp cùng giọng tại máy chủ' : 'Chưa có bộ âm thanh cho cấu hình này');
    const part = amountOnly ? 'amount' : 'full';
    // Choose one cold transport before requesting. Never resynthesize after failure.
    if (typeof window !== 'undefined' && window.Audio) {
      const route = voiceRoute(message, options.isPublic, profile, part);
      transport = await prepareStreamPlayback(route.streamUrl, profile, signal, new window.Audio());
      native = transport.audio;
      return { ...common, kind: 'stream', streamUrl: route.streamUrl, audioElement: native, completed: transport.completed, audioPath: 'cold-stream' };
    }
    const buffer = await coldBuffer(message, profile, part, options, signal, owner);
    return { ...common, kind: 'buffers', segments: [{ buffer, role: 'full' }], audioPath: 'cold-buffer' };
  } catch (error) { release(); throw error; }
}

export function markSpeechStarted(
  message: VoiceMessage,
  options?: {
    startEvidence?: 'audio-clock' | 'media-playing';
    amountOffsetMs?: number | null;
    amountEvidence?: 'reviewed-offset' | 'segment-boundary' | 'unavailable';
    audioPath?: 'decoded-pack' | 'compressed-pack' | 'network-pack' | 'cold-stream' | 'cold-buffer';
    monotonicNow?: number;
  }
): void {
  if (message.telemetry) {
    const now = options?.monotonicNow ?? performance.now();
    message.telemetry.speechStartedAt = Date.now();
    message.telemetry.speechStartedMonotonicAt = now;
    if (options?.audioPath) message.telemetry.audioPath = options.audioPath;
    if (options?.startEvidence) message.telemetry.startEvidence = options.startEvidence;
    if (message.telemetry.receivedMonotonicAt !== undefined) {
      message.telemetry.sseToSpeakMs = now - message.telemetry.receivedMonotonicAt;
    }
    if (message.queuedMonotonicAt !== undefined) {
      message.telemetry.queueToSpeakMs = now - message.queuedMonotonicAt;
    }
    if (options?.amountOffsetMs != null && options.amountOffsetMs >= 0) {
      const amountMonotonic = now + options.amountOffsetMs;
      message.telemetry.amountStartedMonotonicAt = amountMonotonic;
      if (message.telemetry.receivedMonotonicAt !== undefined) {
        message.telemetry.sseToAmountMs = amountMonotonic - message.telemetry.receivedMonotonicAt;
      }
      message.telemetry.amountEvidence = options.amountEvidence ?? 'reviewed-offset';
    } else if (options?.amountEvidence) {
      message.telemetry.amountEvidence = options.amountEvidence;
    } else {
      message.telemetry.amountEvidence = 'unavailable';
    }
  }
  message.onStart?.(message.telemetry);
}

export async function playPrepared(plan: PreparedAnnouncement, signal: AbortSignal, options: PreparationOptions,
    notBefore = 0): Promise<void> {
  throwIfAborted(signal);
  const context = options.context;
  if (!context || context.state !== 'running') {
    options.onStatus?.('unavailable', 'AUDIO_CONTEXT_NOT_RUNNING');
    throw Object.assign(new Error('AUDIO_CONTEXT_NOT_RUNNING'), { playbackStarted: false });
  }
  if (plan.kind === 'stream') {
    const audio = plan.audioElement;
    audio.volume = 1; audio.playbackRate = 1;
    const output = createSpeechOutput(context, plan.volume);
    let source: MediaElementAudioSourceNode | undefined;
    try {
      source = context.createMediaElementSource(audio);
      source.connect(output.input);
      const { promise, resolve, reject } = Promise.withResolvers<void>();
      let started = false, settled = false;
      const finish = (error?: unknown) => {
        if (settled) return;
        settled = true; clearTimeout(timer);
        signal.removeEventListener('abort', aborted);
        context.removeEventListener('statechange', stateChanged);
        audio.removeEventListener('playing', playing); audio.removeEventListener('ended', ended); audio.removeEventListener('error', failed);
        if (error) {
          const original = error instanceof Error ? error : new Error(String(error));
          const permissionDenied = error !== null && typeof error === 'object' && 'name' in error && error.name === 'NotAllowedError';
          const failure = permissionDenied ? new Error('AUDIO_CONTEXT_NOT_RUNNING') : original;
          if (failure.message === 'AUDIO_CONTEXT_NOT_RUNNING') options.onStatus?.('unavailable', failure.message);
          audio.pause();
          reject(Object.assign(failure, { playbackStarted: started }));
        } else resolve();
      };
      const playing = () => { if (!started) { started = true; markSpeechStarted(plan.message, { startEvidence: 'media-playing', audioPath: plan.audioPath ?? 'cold-stream' }); if (plan.recipe?.styleId === 'classic') plan.message.onTailStart?.(plan.recipe.id); } };
      // Native whole-phrase audio has no word-boundary event: completion is the
      // conservative evidence that the tail was heard, never a guessed timestamp.
      const ended = () => { if (plan.recipe && plan.recipe.styleId !== 'classic') plan.message.onTailStart?.(plan.recipe.id); finish(); };
      const aborted = () => { audio.pause(); finish(new Error('VOICE_CANCELLED')); };
      const failed = () => finish(new Error(started ? 'AUDIO_STREAM_INTERRUPTED' : 'AUDIO_PLAYBACK_ERROR'));
      const stateChanged = () => {
        if (context.state === 'running' || settled) return;
        options.onStatus?.('unavailable', 'AUDIO_CONTEXT_NOT_RUNNING');
        finish(new Error('AUDIO_CONTEXT_NOT_RUNNING'));
      };
      const timer = setTimeout(() => finish(new Error('AUDIO_PLAYBACK_TIMEOUT')), 120000);
      signal.addEventListener('abort', aborted, { once: true });
      context.addEventListener('statechange', stateChanged);
      audio.addEventListener('playing', playing); audio.addEventListener('ended', ended); audio.addEventListener('error', failed);
      void plan.completed?.catch((error) => { audio.pause(); finish(error); });
      if (context.state !== 'running') stateChanged();
      else if (signal.aborted) aborted();
      else { try { void audio.play().catch(finish); } catch (error) { finish(error); } }
      await promise;
    } finally { source?.disconnect(); output.disconnect(); }
    return;
  }
  const { promise, resolve, reject } = Promise.withResolvers<void>();
    const sources: AudioBufferSourceNode[] = [];
    const output = createSpeechOutput(context, plan.volume);
    let started = false, settled = false;
    const timers: ReturnType<typeof setTimeout>[] = [];
    const finish = (error?: Error) => {
      if (settled) return;
      settled = true; clearTimeout(watchdog); timers.forEach(clearTimeout); signal.removeEventListener('abort', aborted);
      context.removeEventListener('statechange', stateChanged);
      for (const source of sources) { source.onended = null; try { source.stop(); } catch {} source.disconnect(); }
      output.disconnect();
      if (error) reject(Object.assign(error, { playbackStarted: started })); else resolve();
    };
    const aborted = () => finish(new Error('VOICE_CANCELLED'));
    const stateChanged = () => {
      if (context.state === 'running' || settled) return;
      options.onStatus?.('unavailable', 'AUDIO_CONTEXT_NOT_RUNNING');
      finish(new Error('AUDIO_CONTEXT_NOT_RUNNING'));
    };
    const watchdog = setTimeout(() => finish(new Error('AUDIO_PLAYBACK_TIMEOUT')), 120000);
    signal.addEventListener('abort', aborted, { once: true });
    context.addEventListener('statechange', stateChanged);
    let cursor = Math.max(context.currentTime, notBefore);
    const startAt = cursor;
    const schedule = (buffer: AudioBuffer, end: () => void) => {
      const source = context.createBufferSource(); source.buffer = buffer; source.playbackRate.value = 1;
      source.connect(output.input); source.onended = end; sources.push(source);
      source.start(cursor); cursor += buffer.duration;
    };
    try {
      if (context.state !== 'running') throw new Error('AUDIO_CONTEXT_NOT_RUNNING');
      throwIfAborted(signal);
      let amountStartCursor: number | null = null;
      let tailStartCursor: number | null = null;
      for (let i = 0; i < plan.segments.length; i++) {
        const seg = plan.segments[i];
        if (seg.role === 'amount' && amountStartCursor === null) {
          amountStartCursor = cursor;
        }
        if (seg.role === 'tail' && tailStartCursor === null) {
          tailStartCursor = cursor;
        }
        const isLast = i === plan.segments.length - 1;
        schedule(seg.buffer, isLast ? () => finish() : () => {});
      }
      if (tailStartCursor !== null && plan.recipe) {
        timers.push(setTimeout(() => {
          if (!settled && !signal.aborted && plan.recipe) plan.message.onTailStart?.(plan.recipe.id);
        }, Math.max(0, (tailStartCursor - context.currentTime) * 1000)));
      }
      timers.push(setTimeout(() => {
        if (settled || signal.aborted) return;
        started = true;
        let clockMonotonic = performance.now();
        if (typeof context.getOutputTimestamp === 'function') {
          try {
            const ts = context.getOutputTimestamp();
            if (ts && ts.performanceTime != null && ts.contextTime != null) {
              clockMonotonic = ts.performanceTime + (startAt - ts.contextTime) * 1000;
            }
          } catch {}
        }
        markSpeechStarted(plan.message, {
          startEvidence: 'audio-clock',
          audioPath: plan.audioPath ?? 'decoded-pack',
          amountOffsetMs: plan.amountOffsetMs,
          amountEvidence: plan.amountEvidence,
          monotonicNow: clockMonotonic,
        });
        if (plan.recipe?.styleId === 'classic') plan.message.onTailStart?.(plan.recipe.id);
      }, Math.max(0, (startAt - context.currentTime) * 1000)));
    } catch (error) { finish(error instanceof Error ? error : new Error(String(error))); }
  await promise;
}
