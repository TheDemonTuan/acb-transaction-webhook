const STORAGE_KEY = 'acb.voice.processed-events.v2';
const MAX_ENTRIES = 500;
export const DEFAULT_MAX_REPLAY_AGE_MS = 120_000; // 120 seconds
export const FUTURE_CLOCK_SKEW_MS = 30_000; // 30 seconds

export interface DedupeKeyOptions {
  eventId?: string | null;
  transactionId?: string | null;
  semanticKey?: string | null;
}

export class VoiceDedupe {
  private processedKeys = new Set<string>();
  private reservedKeys = new Set<string>();
  private skippedKeys = new Set<string>();

  constructor() {
    this.loadFromStorage();
  }

  private loadFromStorage() {
    if (typeof window === 'undefined' || !window.sessionStorage) return;
    try {
      const raw = window.sessionStorage.getItem(STORAGE_KEY);
      if (raw) {
        const arr = JSON.parse(raw);
        if (Array.isArray(arr)) {
          arr.forEach((k) => {
            if (typeof k === 'string') this.processedKeys.add(k);
          });
        }
      }
    } catch {
      // Session storage access error fallback to memory
    }
  }

  private saveToStorage() {
    if (typeof window === 'undefined' || !window.sessionStorage) return;
    try {
      const arr = Array.from(this.processedKeys);
      const trimmed = arr.slice(-MAX_ENTRIES);
      window.sessionStorage.setItem(STORAGE_KEY, JSON.stringify(trimmed));
    } catch {
      // Ignore quota/access errors
    }
  }

  public getKeys(opts: DedupeKeyOptions): string[] {
    const keys: string[] = [];
    if (opts.eventId) keys.push(`ev:${opts.eventId}`);
    if (opts.transactionId) keys.push(`tx:${opts.transactionId}`);
    if (opts.semanticKey) keys.push(`sem:${opts.semanticKey}`);
    return keys;
  }

  public has(opts: DedupeKeyOptions): boolean {
    const keys = this.getKeys(opts);
    if (keys.length === 0) return false;
    return keys.some((k) => this.processedKeys.has(k) || this.skippedKeys.has(k));
  }

  public isReserved(opts: DedupeKeyOptions): boolean {
    const keys = this.getKeys(opts);
    if (keys.length === 0) return false;
    return keys.some((k) => this.reservedKeys.has(k));
  }

  public reserve(opts: DedupeKeyOptions): boolean {
    if (this.has(opts) || this.isReserved(opts)) {
      return false;
    }
    const keys = this.getKeys(opts);
    if (keys.length === 0) return false;
    keys.forEach((k) => this.reservedKeys.add(k));
    return true;
  }

  public commit(opts: DedupeKeyOptions): void {
    const keys = this.getKeys(opts);
    if (keys.length === 0) return;

    keys.forEach((k) => {
      this.reservedKeys.delete(k);
      this.processedKeys.add(k);
    });

    if (this.processedKeys.size > MAX_ENTRIES * 1.5) {
      const arr = Array.from(this.processedKeys).slice(-MAX_ENTRIES);
      this.processedKeys = new Set(arr);
    }
    this.saveToStorage();
  }

  public release(opts: DedupeKeyOptions): void {
    const keys = this.getKeys(opts);
    keys.forEach((k) => this.reservedKeys.delete(k));
  }

  public markSkipped(opts: DedupeKeyOptions, _reason?: string): void {
    const keys = this.getKeys(opts);
    keys.forEach((k) => {
      this.reservedKeys.delete(k);
      this.skippedKeys.add(k);
    });
    if (this.skippedKeys.size > MAX_ENTRIES * 1.5) {
      const arr = Array.from(this.skippedKeys).slice(-MAX_ENTRIES);
      this.skippedKeys = new Set(arr);
    }
  }

  // Legacy convenience method
  public mark(opts: DedupeKeyOptions): void {
    this.commit(opts);
  }

  public isFreshEnough(
    detectedAtStr?: string,
    maxAgeMs = DEFAULT_MAX_REPLAY_AGE_MS,
    referenceTimeMs = Date.now()
  ): boolean {
    if (!detectedAtStr) return false;
    const detected = Date.parse(detectedAtStr);
    if (Number.isNaN(detected)) return false;

    const age = referenceTimeMs - detected;
    // Allow future clock skew up to 30s (-30_000ms <= age <= maxAgeMs)
    if (age < -FUTURE_CLOCK_SKEW_MS || age > maxAgeMs) {
      return false;
    }
    return true;
  }

  public clear(): void {
    this.processedKeys.clear();
    this.reservedKeys.clear();
    this.skippedKeys.clear();
    if (typeof window !== 'undefined' && window.sessionStorage) {
      try {
        window.sessionStorage.removeItem(STORAGE_KEY);
      } catch {}
    }
  }
}
