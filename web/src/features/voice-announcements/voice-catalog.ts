export interface CatalogFragment {
  id: string;
  text: string;
  styleId: string;
  bandId: string;
}
export interface CatalogRecipe {
  id: string;
  styleId: string;
  bandId: string;
  openingId: string;
  closingId: string;
  weight: number;
}
export interface VoiceCatalog {
  version: 'v1';
  amountCore: string;
  fragments: CatalogFragment[];
  recipes: CatalogRecipe[];
  classic: { id: string; template: string }[];
}

export function throwIfAborted(signal: AbortSignal): void {
  if (signal.aborted) throw new Error('VOICE_CANCELLED');
}

// Read incrementally: Content-Length alone does not bound chunked bodies.
export async function readBounded(response: Response, cap: number, signal: AbortSignal): Promise<ArrayBuffer> {
  throwIfAborted(signal);
  if (Number(response.headers.get('Content-Length')) > cap) {
    await response.body?.cancel();
    throw new Error('VOICE_RESPONSE_TOO_LARGE');
  }
  if (!response.body) throw new Error('VOICE_EMPTY_RESPONSE');
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  const abort = () => { void reader.cancel().catch(() => {}); };
  signal.addEventListener('abort', abort, { once: true });
  try {
    for (;;) {
      const { value, done } = await reader.read();
      throwIfAborted(signal);
      if (done) break;
      length += value.byteLength;
      if (length > cap) throw new Error('VOICE_RESPONSE_TOO_LARGE');
      chunks.push(value);
    }
    const result = new Uint8Array(length);
    let offset = 0;
    for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength; }
    return result.buffer;
  } finally {
    signal.removeEventListener('abort', abort);
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

const downloads = new Map<string, Promise<VoiceCatalog | null>>();
export function loadVoiceCatalog(version: 'v1', isPublic: boolean, signal?: AbortSignal): Promise<VoiceCatalog | null> {
  if (signal?.aborted) return Promise.reject(new Error('VOICE_CANCELLED'));
  const key = `${isPublic}:${version}`;
  let pending = downloads.get(key);
  if (!pending) {
    pending = (async () => {
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), 8000);
      try {
        const response = await fetch(`${isPublic ? '/api/public/v1' : '/api/v1'}/voice/catalog/${version}`, {
          credentials: 'same-origin', signal: controller.signal, headers: { Accept: 'application/json' },
        });
        if (!response.ok) return null;
        const data: unknown = JSON.parse(new TextDecoder().decode(await readBounded(response, 4 * 1024 * 1024, controller.signal)));
        return validateCatalog(data, version);
      } catch { return null; }
      finally { clearTimeout(timeout); }
    })();
    downloads.set(key, pending);
  }
  if (!signal) return pending;
  // A caller owns its wait, not the shared immutable download used by other plans.
  const { promise, resolve, reject } = Promise.withResolvers<VoiceCatalog | null>();
  const abort = () => reject(new Error('VOICE_CANCELLED'));
  signal.addEventListener('abort', abort, { once: true });
  void pending.then(resolve, reject).finally(() => signal.removeEventListener('abort', abort));
  return promise;
}

export function validateCatalog(data: unknown, version: 'v1'): VoiceCatalog {
  const c = data as VoiceCatalog;
  if (!c || c.version !== version || c.amountCore !== 'Đã nhận {amount}.' ||
      !Array.isArray(c.fragments) || !Array.isArray(c.recipes) || !Array.isArray(c.classic)) {
    throw new Error('VOICE_CATALOG_INVALID');
  }
  const ids = new Set<string>();
  const fragments = new Map<string, CatalogFragment>();
  for (const f of c.fragments) {
    if (!f || typeof f.id !== 'string' || ids.has(f.id) || typeof f.text !== 'string' || !f.text.trim() ||
        !['cute', 'funny'].includes(f.styleId) || !['small', 'medium', 'large', 'celebrate'].includes(f.bandId)) {
      throw new Error('VOICE_CATALOG_INVALID');
    }
    ids.add(f.id); fragments.set(f.id, f);
  }
  const classic = new Map(c.classic.map((f) => [f.id, f.template]));
  ids.clear();
  for (const r of c.recipes) {
    if (!r || typeof r.id !== 'string' || ids.has(r.id) || !Number.isSafeInteger(r.weight) || r.weight <= 0) {
      throw new Error('VOICE_CATALOG_INVALID');
    }
    ids.add(r.id);
    if (r.styleId === 'classic') {
      if (r.bandId !== 'all' || typeof classic.get(r.id) !== 'string') throw new Error('VOICE_CATALOG_INVALID');
    } else {
      const opening = fragments.get(r.openingId), closing = fragments.get(r.closingId);
      if (!opening || !closing || opening.styleId !== r.styleId || closing.styleId !== r.styleId ||
          opening.bandId !== r.bandId || closing.bandId !== r.bandId) throw new Error('VOICE_CATALOG_INVALID');
    }
  }
  return c;
}
