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
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  const abort = () => { controller.abort(); void reader?.cancel().catch(() => {}); };
  signal.addEventListener('abort', abort, { once: true });
  if (signal.aborted) abort();
  let timedOut = false;
  const timer = setTimeout(() => { timedOut = true; abort(); }, profile.coldRequestTimeoutMs);
  let objectURL: string | undefined;
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
        headers.get('X-TTS-Fallback') !== 'false') {
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
    const readiness = Promise.withResolvers<void>();
    let downloadDone = false;
    let mediaReady = false;
    const clearWhenReady = () => { if (downloadDone && mediaReady) clearTimeout(timer); };
    const markReady = () => { mediaReady = true; clearWhenReady(); readiness.resolve(); };
    const playable = readiness.promise;
    // Own readiness even if a body failure wins the handoff race.
    void playable.catch(() => {});
    if (sourceBuffer) {
      void waitEvent(audio, 'canplay', controller.signal).then(markReady, readiness.reject);
    }
    const completed = (async () => {
      try {
        let total = 0;
        const chunks: Uint8Array<ArrayBuffer>[] = [];
        while (true) {
          if (controller.signal.aborted) throw new Error('VOICE_CANCELLED');
          const { done, value } = await reader!.read();
          if (controller.signal.aborted) throw new Error('VOICE_CANCELLED');
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
        downloadDone = true;
        clearWhenReady();
        if (mediaSource) mediaSource.endOfStream();
        else {
          objectURL = URL.createObjectURL(new Blob(chunks, { type: 'audio/mpeg' }));
          const ready = waitEvent(audio, 'canplay', controller.signal);
          audio.src = objectURL; audio.load();
          void ready.then(markReady, readiness.reject);
        }
      } catch (error) {
        if (timedOut) throw new Error('TTS_TIMEOUT');
        if (signal.aborted) throw new Error('VOICE_CANCELLED');
        throw error;
      }
    })();
    void completed.catch(release);
    // EOF is not evidence of media readiness. A failed body still rejects promptly.
    await Promise.race([playable, completed.then(() => playable)]);
    if (controller.signal.aborted) throw new Error(signal.aborted ? 'VOICE_CANCELLED' : 'TTS_TIMEOUT');
    return { audio, completed, release };
  } catch (error) {
    release();
    if (timedOut) throw new Error('TTS_TIMEOUT');
    if (signal.aborted) throw new Error('VOICE_CANCELLED');
    throw error;
  }
}
