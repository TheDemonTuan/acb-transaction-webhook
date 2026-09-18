import { REALTIME_EVENT_TYPES } from './realtime.events';
import type {
  RealtimeDiagnostics,
  RealtimeEnvelope,
  RealtimeEventType,
  RealtimeListener,
  RealtimeStatus,
} from './realtime.types';

export interface RealtimeClientOptions {
  url?: string;
  probeUrl?: string;
  staleThresholdMs?: number;
  heartbeatCheckIntervalMs?: number;
  minReconnectDelayMs?: number;
  maxReconnectDelayMs?: number;
  onStatusChange?: (status: RealtimeStatus) => void;
  onDiagnosticsChange?: (diag: RealtimeDiagnostics) => void;
  onInitialState?: (watermark: number) => void;
  onResetState?: (reason: string) => void;
  onError?: (error: unknown) => void;
}

export class RealtimeClient {
  private url: string;
  private eventSource: EventSource | null = null;
  private status: RealtimeStatus = 'DISCONNECTED';
  private lastHeartbeatAt: number | null = null;
  private lastMessageAt: number | null = null;
  private lastEventId: string | null = null;
  private connectedAt: number | null = null;
  private disconnectedAt: number | null = null;
  private reconnectCount = 0;
  private lastError: unknown | null = null;
  private networkOnline = true;
  private serverReachable = true;

  private listeners = new Map<string, Set<RealtimeListener<any>>>();
  private wildcardListeners = new Set<RealtimeListener<any>>();
  private options: RealtimeClientOptions;
  private disposed = false;

  private watchdogTimer: ReturnType<typeof setInterval> | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectDelay: number;
  private lastVisibleTick = Date.now();

  private boundVisibilityHandler: (() => void) | null = null;
  private boundOnlineHandler: (() => void) | null = null;
  private boundOfflineHandler: (() => void) | null = null;
  private boundPageshowHandler: (() => void) | null = null;
  private boundFocusHandler: (() => void) | null = null;

  constructor(options: RealtimeClientOptions = {}) {
    this.url = options.url || '/api/v1/events';
    this.options = options;
    this.reconnectDelay = options.minReconnectDelayMs ?? 1000;
    if (typeof navigator !== 'undefined' && typeof navigator.onLine === 'boolean') {
      this.networkOnline = navigator.onLine;
    }
  }

  public getStatus(): RealtimeStatus {
    return this.status;
  }

  public getLastHeartbeatAt(): number | null {
    return this.lastHeartbeatAt;
  }

  public getLastEventId(): string | null {
    return this.lastEventId;
  }

  public getDiagnostics(): RealtimeDiagnostics {
    return {
      status: this.status,
      lastHeartbeatAt: this.lastHeartbeatAt,
      lastMessageAt: this.lastMessageAt,
      lastEventId: this.lastEventId,
      connectedAt: this.connectedAt,
      disconnectedAt: this.disconnectedAt,
      reconnectCount: this.reconnectCount,
      lastError: this.lastError,
      networkOnline: this.networkOnline,
      serverReachable: this.serverReachable,
    };
  }

  private setStatus(next: RealtimeStatus) {
    if (this.status !== next) {
      this.status = next;
      this.options.onStatusChange?.(next);
      this.notifyDiagnostics();
    }
  }

  private notifyDiagnostics() {
    this.options.onDiagnosticsChange?.(this.getDiagnostics());
  }

  private recordActivity() {
    this.lastMessageAt = Date.now();
  }

  private isHeartbeatStale(): boolean {
    const threshold = this.options.staleThresholdMs ?? 12000;
    const baseline = Math.max(
      this.lastHeartbeatAt ?? 0,
      this.connectedAt ?? 0,
    );
    return baseline > 0 && Date.now() - baseline > threshold;
  }

  public connect(): void {
    if (typeof window === 'undefined' || typeof EventSource === 'undefined') {
      return;
    }
    if (this.eventSource) {
      return;
    }

    this.disposed = false;
    this.attachDomListeners();
    this.startWatchdog();
    this.doConnect();
  }

  private getConnectUrl(): string {
    if (!this.lastEventId) {
      return this.url;
    }
    try {
      const base =
        typeof window !== 'undefined' && window.location?.origin
          ? window.location.origin
          : 'http://localhost';
      const parsed = new URL(this.url, base);
      parsed.searchParams.set('lastEventId', this.lastEventId);
      return parsed.pathname + parsed.search;
    } catch {
      const sep = this.url.includes('?') ? '&' : '?';
      return `${this.url}${sep}lastEventId=${encodeURIComponent(this.lastEventId)}`;
    }
  }

  private doConnect(): void {
    if (this.disposed || this.eventSource) {
      return;
    }
    if (typeof navigator !== 'undefined' && navigator.onLine === false) {
      this.networkOnline = false;
      this.setStatus('DISCONNECTED');
      return;
    }

    this.setStatus(this.reconnectCount > 0 ? 'RECONNECTING' : 'CONNECTING');
    try {
      const connectUrl = this.getConnectUrl();
      const source = new EventSource(connectUrl);
      this.eventSource = source;
      const isCurrent = () => !this.disposed && this.eventSource === source;

      source.onopen = () => {
        if (isCurrent()) {
          this.setStatus('CONNECTED');
          this.connectedAt = Date.now();
          this.reconnectDelay = this.options.minReconnectDelayMs ?? 1000;
          this.serverReachable = true;
          this.lastError = null;
          this.notifyDiagnostics();
        }
      };

      source.onerror = (err) => {
        if (!isCurrent()) return;
        this.lastError = err;
        this.options.onError?.(err);

        if (typeof navigator !== 'undefined' && !navigator.onLine) {
          this.networkOnline = false;
          this.setStatus('DISCONNECTED');
        } else {
          this.setStatus('RECONNECTING');
          void this.probeGateway();
        }

        // Close failing source before scheduling reconnect
        source.close();
        if (this.eventSource === source) {
          this.eventSource = null;
        }
        this.scheduleReconnect();
      };

      source.addEventListener('initial_state', (e: MessageEvent) => {
        if (!isCurrent()) return;
        this.recordActivity();
        try {
          const parsed = JSON.parse(e.data);
          const wm = typeof parsed?.watermark === 'number' ? parsed.watermark : 0;
          if (parsed?.epoch && wm > 0 && !this.lastEventId) {
            this.lastEventId = `${parsed.epoch}:${wm}`;
          }
          this.options.onInitialState?.(wm);
        } catch {
          this.options.onInitialState?.(0);
        }
        this.notifyDiagnostics();
      });

      source.addEventListener('reset_state', (e: MessageEvent) => {
        if (!isCurrent()) return;
        this.recordActivity();
        let reason = 'unknown';
        try {
          const parsed = JSON.parse(e.data);
          reason = parsed?.reason || 'unknown';
        } catch {}
        this.options.onResetState?.(reason);
        // Clear stale cursor to avoid retention_expired reconnect loop
        this.lastEventId = null;
        source.close();
        if (this.eventSource === source) {
          this.eventSource = null;
        }
        this.notifyDiagnostics();
        this.scheduleReconnect(500);
      });

      source.addEventListener('stream.heartbeat', (e: MessageEvent) => {
        if (!isCurrent()) return;
        const now = Date.now();
        this.lastHeartbeatAt = now;
        this.recordActivity();
        if (this.status === 'STALE' || this.status === 'CONNECTING' || this.status === 'RECONNECTING') {
          this.setStatus('CONNECTED');
        }
        this.notifyDiagnostics();
        this.dispatch('stream.heartbeat', e);
      });

      for (const type of REALTIME_EVENT_TYPES) {
        if (type === 'stream.heartbeat') continue;
        source.addEventListener(type, (e: MessageEvent) => {
          if (!isCurrent()) return;
          this.recordActivity();
          this.dispatch(type, e);
        });
      }
    } catch (err) {
      this.eventSource = null;
      this.lastError = err;
      this.setStatus('DISCONNECTED');
      this.options.onError?.(err);
      this.scheduleReconnect();
    }
  }

  private scheduleReconnect(explicitDelay?: number): void {
    if (this.disposed || this.reconnectTimer) return;
    const delay = explicitDelay ?? this.reconnectDelay;
    const maxDelay = this.options.maxReconnectDelayMs ?? 10000;
    this.reconnectDelay = Math.min(maxDelay, Math.round(this.reconnectDelay * 1.5));

    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      if (!this.disposed && !this.eventSource) {
        this.reconnectCount++;
        this.doConnect();
      }
    }, delay);
  }

  public hardReconnect(): void {
    if (this.disposed) return;
    this.lastVisibleTick = Date.now();
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
    this.setStatus('RECONNECTING');
    this.reconnectCount++;
    this.notifyDiagnostics();
    this.doConnect();
  }

  public ensureHealthy(force = false): void {
    if (this.disposed) return;
    if (typeof navigator !== 'undefined' && navigator.onLine === false) {
      this.networkOnline = false;
      this.setStatus('DISCONNECTED');
      return;
    }
    this.networkOnline = true;

    if (force) {
      this.hardReconnect();
      return;
    }

    if (this.status === 'CONNECTED') {
      if (this.isHeartbeatStale()) {
        this.setStatus('STALE');
        this.hardReconnect();
      }
      return;
    }

    if (this.status === 'STALE' || this.status === 'RECONNECTING' || this.status === 'DISCONNECTED') {
      this.hardReconnect();
    }
  }

  private async probeGateway(): Promise<void> {
    if (typeof window === 'undefined' || typeof fetch === 'undefined') return;
    if (typeof navigator !== 'undefined' && navigator.onLine === false) {
      this.networkOnline = false;
      this.serverReachable = false;
      this.notifyDiagnostics();
      return;
    }
    const probeUrl =
      this.options.probeUrl ||
      (this.url.includes('/public/') ? '/api/public/v1/payment-qr' : '/healthz');
    try {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), 2000);
      const res = await fetch(probeUrl, {
        method: 'GET',
        signal: controller.signal,
        cache: 'no-store',
        credentials: 'same-origin',
      });
      clearTimeout(timer);
      this.serverReachable = res.ok || (res.status >= 200 && res.status < 500);
    } catch {
      this.serverReachable = false;
    }
    this.notifyDiagnostics();
  }

  private startWatchdog(): void {
    if (this.watchdogTimer) return;
    const interval = this.options.heartbeatCheckIntervalMs ?? 2500;
    this.lastVisibleTick = Date.now();

    this.watchdogTimer = setInterval(() => {
      if (this.disposed) return;
      const now = Date.now();
      const gap = now - this.lastVisibleTick;
      this.lastVisibleTick = now;

      // Laptop sleep detected (timer frozen for > 10s)
      if (gap > 10000) {
        this.ensureHealthy(true);
        return;
      }

      // Check for stale connection
      if (this.status === 'CONNECTED' && this.isHeartbeatStale()) {
        this.setStatus('STALE');
        this.hardReconnect();
      }
    }, interval);
  }

  private stopWatchdog(): void {
    if (this.watchdogTimer) {
      clearInterval(this.watchdogTimer);
      this.watchdogTimer = null;
    }
  }

  private attachDomListeners(): void {
    if (typeof window === 'undefined' || typeof document === 'undefined') return;

    if (!this.boundVisibilityHandler) {
      this.boundVisibilityHandler = () => {
        if (document.visibilityState === 'visible') {
          const elapsed = Date.now() - this.lastVisibleTick;
          this.lastVisibleTick = Date.now();
          if (elapsed > 10000 || this.status !== 'CONNECTED' || this.isHeartbeatStale()) {
            this.ensureHealthy(true);
          } else {
            this.ensureHealthy(false);
          }
        }
      };
      document.addEventListener('visibilitychange', this.boundVisibilityHandler);
    }

    if (!this.boundPageshowHandler) {
      this.boundPageshowHandler = () => {
        this.ensureHealthy(true);
      };
      window.addEventListener('pageshow', this.boundPageshowHandler);
    }

    if (!this.boundOnlineHandler) {
      this.boundOnlineHandler = () => {
        this.networkOnline = true;
        this.notifyDiagnostics();
        this.hardReconnect();
      };
      window.addEventListener('online', this.boundOnlineHandler);
    }

    if (!this.boundOfflineHandler) {
      this.boundOfflineHandler = () => {
        this.networkOnline = false;
        this.setStatus('DISCONNECTED');
        if (this.eventSource) {
          this.eventSource.close();
          this.eventSource = null;
        }
        this.notifyDiagnostics();
      };
      window.addEventListener('offline', this.boundOfflineHandler);
    }

    if (!this.boundFocusHandler) {
      this.boundFocusHandler = () => {
        this.ensureHealthy(false);
      };
      window.addEventListener('focus', this.boundFocusHandler);
    }
  }

  private detachDomListeners(): void {
    if (typeof window === 'undefined' || typeof document === 'undefined') return;

    if (this.boundVisibilityHandler) {
      document.removeEventListener('visibilitychange', this.boundVisibilityHandler);
      this.boundVisibilityHandler = null;
    }
    if (this.boundPageshowHandler) {
      window.removeEventListener('pageshow', this.boundPageshowHandler);
      this.boundPageshowHandler = null;
    }
    if (this.boundOnlineHandler) {
      window.removeEventListener('online', this.boundOnlineHandler);
      this.boundOnlineHandler = null;
    }
    if (this.boundOfflineHandler) {
      window.removeEventListener('offline', this.boundOfflineHandler);
      this.boundOfflineHandler = null;
    }
    if (this.boundFocusHandler) {
      window.removeEventListener('focus', this.boundFocusHandler);
      this.boundFocusHandler = null;
    }
  }

  private dispatch(type: string, event: MessageEvent) {
    if (event.lastEventId) {
      this.lastEventId = event.lastEventId;
    }

    let data: unknown;
    try {
      data = JSON.parse(event.data);
    } catch {
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
    this.stopWatchdog();
    this.detachDomListeners();

    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }

    this.setStatus('DISCONNECTED');
    this.disconnectedAt = Date.now();
    this.listeners.clear();
    this.wildcardListeners.clear();
    this.notifyDiagnostics();
  }
}
