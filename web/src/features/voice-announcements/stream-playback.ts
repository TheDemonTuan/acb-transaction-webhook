import { type VoiceProfile, synthesisProvider } from './voice-profile';

const MAX_STREAM_BYTES = 4 * 1024 * 1024;

export interface StreamPlayback {
  audio: HTMLAudioElement;
  completed: Promise<void>;
  release: () => void;
}

function waitEvent(target: EventTarget, event: string, signal: AbortSignal): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  const clean = () => {
    target.removeEventListener(event, ready);
    target.removeEventListener('error', failed);
    signal.removeEventListener('abort', aborted);
  };
  const ready = () => { clean(); resolve(); };
  const failed = () => { clean(); reject(new Error('AUDIO_PREPARATION_FAILED')); };
  const aborted = () => { clean(); reject(new Error('VOICE_CANCELLED')); };
  target.addEventListener(event, ready, { once: true });
  target.addEventListener('error', failed, { once: true });
  signal.addEventListener('abort', aborted, { once: true });
  if (signal.aborted) aborted();
  return promise;
}

export async function prepareStreamPlayback(
  url: string, profile: Readonly<VoiceProfile>, signal: AbortSignal, audio: HTMLAudioElement,
): Promise<StreamPlayback> {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener('abort', abort, { once: true });
  if (signal.aborted) abort();
  let timedOut = false;
  const timer = setTimeout(() => { timedOut = true; abort(); }, profile.coldRequestTimeoutMs);
  let objectURL: string | undefined;
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  let released = false;
  const release = () => {
    if (released) return;
    released = true;
    controller.abort();
    clearTimeout(timer);
    signal.removeEventListener('abort', abort);
    void reader?.cancel().catch(() => {});
    audio.pause(); audio.removeAttribute('src'); audio.load();
    if (objectURL) URL.revokeObjectURL(objectURL);
  };
  try {
    const response = await fetch(url, { credentials: 'same-origin', signal: controller.signal });
    if (!response.ok) throw new Error(`TTS_STREAM_HTTP_${response.status}`);
    const headers = response.headers;
    if (headers.get('X-Voice-Revision') !== String(profile.revision) ||
        headers.get('X-Voice-Synthesis-Key') !== profile.synthesisKey ||
        headers.get('X-TTS-Voice') !== profile.voiceId ||
        headers.get('X-TTS-Provider') !== synthesisProvider(profile.providerMode) ||
        headers.get('X-TTS-Fallback') === 'true') {
      await response.body?.cancel();
      throw new Error('VOICE_AUDIO_PROFILE_MISMATCH');
    }
    if (!response.body) throw new Error('TTS_EMPTY_STREAM_BODY');
    reader = response.body.getReader();
    let mediaSource: MediaSource | undefined;
    let sourceBuffer: SourceBuffer | undefined;
    if (typeof MediaSource !== 'undefined' && MediaSource.isTypeSupported('audio/mpeg')) {
      mediaSource = new MediaSource();
      const opened = waitEvent(mediaSource, 'sourceopen', controller.signal);
      objectURL = URL.createObjectURL(mediaSource);
      audio.src = objectURL; audio.load();
      await opened;
      // Select buffered playback only before consuming bytes; never request synthesis twice.
      try { sourceBuffer = mediaSource.addSourceBuffer('audio/mpeg'); }
      catch { URL.revokeObjectURL(objectURL); objectURL = undefined; mediaSource = undefined; }
    }
    const playable = sourceBuffer ? waitEvent(audio, 'canplay', controller.signal) : undefined;
    // Both promises are owned even when preparation fails before returning a plan.
    void playable?.catch(() => {});
    const completed = (async () => {
      let total = 0;
      const chunks: Uint8Array<ArrayBuffer>[] = [];
      while (true) {
        const { done, value } = await reader!.read();
        if (done) break;
        if (!value.byteLength) continue;
        total += value.byteLength;
        if (total > MAX_STREAM_BYTES) throw new Error('TTS_STREAM_TOO_LARGE');
        if (sourceBuffer) {
          if (sourceBuffer.updating) await waitEvent(sourceBuffer, 'updateend', controller.signal);
          const updated = waitEvent(sourceBuffer, 'updateend', controller.signal);
          void updated.catch(() => {});
          sourceBuffer.appendBuffer(value as Uint8Array<ArrayBuffer>);
          await updated;
        } else chunks.push(value as Uint8Array<ArrayBuffer>);
      }
      if (!total) throw new Error('TTS_EMPTY_STREAM');
      if (mediaSource) mediaSource.endOfStream();
      else {
        objectURL = URL.createObjectURL(new Blob(chunks, { type: 'audio/mpeg' }));
        const ready = waitEvent(audio, 'canplay', controller.signal);
        audio.src = objectURL; audio.load();
        await ready;
      }
      clearTimeout(timer);
    })();
    void completed.catch(() => { audio.pause(); clearTimeout(timer); controller.abort(); });
    await (playable ? Promise.race([playable, completed]) : completed);
    if (controller.signal.aborted) throw new Error(signal.aborted ? 'VOICE_CANCELLED' : 'TTS_TIMEOUT');
    return { audio, completed, release };
  } catch (error) {
    release();
    if (timedOut) throw new Error('TTS_TIMEOUT');
    if (signal.aborted) throw new Error('VOICE_CANCELLED');
    throw error;
  }
}
