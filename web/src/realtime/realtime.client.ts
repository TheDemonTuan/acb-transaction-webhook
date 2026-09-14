import { REALTIME_EVENT_TYPES } from './realtime.events';
import type { RealtimeEnvelope, RealtimeEventType, RealtimeListener, RealtimeStatus } from './realtime.types';

export interface RealtimeClientOptions {
  url?: string;
  onStatusChange?: (status: RealtimeStatus) => void;
  onInitialState?: (watermark: number) => void;
  onResetState?: (reason: string) => void;
  onError?: (error: unknown) => void;
}

export class RealtimeClient {
  private url: string;
  private eventSource: EventSource | null = null;
  private status: RealtimeStatus = 'DISCONNECTED';
  private listeners = new Map<string, Set<RealtimeListener<any>>>();
  private wildcardListeners = new Set<RealtimeListener<any>>();
  private options: RealtimeClientOptions;
  private disposed = false;

  constructor(options: RealtimeClientOptions = {}) {
    this.url = options.url || '/api/v1/events';
    this.options = options;
  }

  public getStatus(): RealtimeStatus {
    return this.status;
  }

  private setStatus(next: RealtimeStatus) {
    if (this.status !== next) {
      this.status = next;
      this.options.onStatusChange?.(next);
    }
  }

  public connect(): void {
    if (typeof window === 'undefined' || typeof EventSource === 'undefined') {
      return;
    }
    if (this.eventSource) {
      return;
    }

    this.disposed = false;
    this.setStatus('CONNECTING');
    try {
      const source = new EventSource(this.url);
      this.eventSource = source;
      const isCurrent = () => !this.disposed && this.eventSource === source;

      source.onopen = () => {
        if (isCurrent()) {
          this.setStatus('CONNECTED');
        }
      };

      source.onerror = (err) => {
        if (isCurrent()) {
          this.setStatus('RECONNECTING');
          this.options.onError?.(err);
        }
      };

      source.addEventListener('initial_state', (e: MessageEvent) => {
        if (!isCurrent()) return;
        try {
          const parsed = JSON.parse(e.data);
          this.options.onInitialState?.(typeof parsed?.watermark === 'number' ? parsed.watermark : 0);
        } catch {
          this.options.onInitialState?.(0);
        }
      });

      source.addEventListener('reset_state', (e: MessageEvent) => {
        if (!isCurrent()) return;
        let reason = 'unknown';
        try {
          const parsed = JSON.parse(e.data);
          reason = parsed?.reason || 'unknown';
        } catch {}
        this.options.onResetState?.(reason);

        // Close stale cursor stream and reconnect fresh to avoid retention_expired loops
        source.close();
        if (this.eventSource === source) {
          this.eventSource = null;
        }
        setTimeout(() => {
          if (!this.disposed && !this.eventSource) {
            this.connect();
          }
        }, 500);
      });

      for (const type of REALTIME_EVENT_TYPES) {
        source.addEventListener(type, (e: MessageEvent) => {
          if (!isCurrent()) return;
          this.dispatch(type, e);
        });
      }
    } catch (err) {
      this.eventSource = null;
      this.setStatus('DISCONNECTED');
      this.options.onError?.(err);
    }
  }

  private dispatch(type: string, event: MessageEvent) {
    let data: unknown;
    try {
      data = JSON.parse(event.data);
    } catch {
      // Malformed event should not crash or trigger subscribers with invalid data
      return;
    }

    const envelope: RealtimeEnvelope = {
      id: event.lastEventId || null,
      type,
      data,
      receivedAt: Date.now(),
    };

    const specific = this.listeners.get(type);
    if (specific) {
      specific.forEach((listener) => {
        try {
          listener(envelope);
        } catch (err) {
          console.error(`[RealtimeClient] Error in listener for ${type}:`, err);
        }
      });
    }

    this.wildcardListeners.forEach((listener) => {
      try {
        listener(envelope);
      } catch (err) {
        console.error(`[RealtimeClient] Error in wildcard listener:`, err);
      }
    });
  }

  public subscribe<T = unknown>(type: RealtimeEventType, listener: RealtimeListener<T>): () => void {
    if (!this.listeners.has(type)) {
      this.listeners.set(type, new Set());
    }
    this.listeners.get(type)!.add(listener as RealtimeListener<any>);
    return () => {
      this.listeners.get(type)?.delete(listener as RealtimeListener<any>);
    };
  }

  public onAny(listener: RealtimeListener<any>): () => void {
    this.wildcardListeners.add(listener);
    return () => {
      this.wildcardListeners.delete(listener);
    };
  }

  public disconnect(): void {
    this.disposed = true;
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
    this.setStatus('DISCONNECTED');
    this.listeners.clear();
    this.wildcardListeners.clear();
  }
}
