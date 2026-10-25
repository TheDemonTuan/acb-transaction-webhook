import calmUrl from './assets/Calm.wav';

/** Bundled, unchanged Calm recording. Source and license: assets/Calm.LICENSE.txt. */
export const CHIME_ASSET_URL = calmUrl;

/** Only explicit unlock loads the recording; playback never starts a network request. */
export async function loadChimeRecording(context: BaseAudioContext, signal?: AbortSignal): Promise<AudioBuffer> {
  const cancelled = Promise.withResolvers<never>();
  const abort = () => cancelled.reject(new Error('VOICE_CANCELLED'));
  signal?.addEventListener('abort', abort, { once: true });
  const loading = (async () => {
    if (signal?.aborted) throw new Error('VOICE_CANCELLED');
    const response = await fetch(CHIME_ASSET_URL, { signal });
    if (!response.ok) throw new Error('CHIME_ASSET_UNAVAILABLE');
    const bytes = await response.arrayBuffer();
    if (signal?.aborted) throw new Error('VOICE_CANCELLED');
    const buffer = await context.decodeAudioData(bytes);
    if (signal?.aborted) throw new Error('VOICE_CANCELLED');
    if (buffer.length === 0 || !Number.isFinite(buffer.duration) || buffer.duration <= 0) {
      throw new Error('CHIME_ASSET_UNAVAILABLE');
    }
    return buffer;
  })();
  if (signal?.aborted) abort();
  try { return await Promise.race([loading, cancelled.promise]); }
  finally { signal?.removeEventListener('abort', abort); }
}
