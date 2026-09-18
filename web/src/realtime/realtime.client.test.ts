import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { RealtimeClient } from './realtime.client';

class FakeEventSource {
  static instances: FakeEventSource[] = [];

  readonly listeners = new Map<string, Array<(event: MessageEvent) => void>>();
  onopen: (() => void) | null = null;
  onerror: ((error: unknown) => void) | null = null;
  closed = false;

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: (event: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  close() {
    this.closed = true;
  }

  emit(type: string, data: unknown, lastEventId?: string) {
    const event = {
      data: JSON.stringify(data),
      lastEventId: lastEventId ?? 'ep1:1',
    } as MessageEvent;
    for (const listener of this.listeners.get(type) ?? []) {
      listener(event);
    }
  }
}

describe('RealtimeClient lifecycle', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    FakeEventSource.instances = [];
    vi.stubGlobal('window', {
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      location: { origin: 'http://localhost' },
    });
    vi.stubGlobal('document', {
      visibilityState: 'visible',
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    });
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  afterEach(() => {
    vi.clearAllTimers();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('reconnects after an effect cleanup without accepting events from the old source', () => {
    const listener = vi.fn();
    const client = new RealtimeClient();

    client.connect();
    const first = FakeEventSource.instances[0];
    client.disconnect();
    client.connect();
    client.subscribe('poll.completed', listener);
    const second = FakeEventSource.instances[1];

    expect(first.closed).toBe(true);
    expect(second.closed).toBe(false);
    expect(FakeEventSource.instances).toHaveLength(2);

    first.emit('poll.completed', { status: 'SUCCEEDED' });
    second.emit('poll.completed', { status: 'SUCCEEDED' });

    expect(listener).toHaveBeenCalledTimes(1);
    client.disconnect();
  });

  it('does not open a duplicate source while connected', () => {
    const client = new RealtimeClient();

    client.connect();
    client.connect();

    expect(FakeEventSource.instances).toHaveLength(1);
    client.disconnect();
  });

  it('preserves lastEventId and reconnects with cursor query param', () => {
    const client = new RealtimeClient({ url: '/api/v1/events' });
    client.connect();
    const source1 = FakeEventSource.instances[0];
    source1.onopen?.();

    expect(client.getStatus()).toBe('CONNECTED');

    // Emit event with lastEventId
    source1.emit('bank.transaction.credit', { transactionId: 'tx1' }, 'ep1:18294');
    expect(client.getLastEventId()).toBe('ep1:18294');

    // Trigger hardReconnect
    client.hardReconnect();
    expect(source1.closed).toBe(true);
    expect(FakeEventSource.instances).toHaveLength(2);

    const source2 = FakeEventSource.instances[1];
    expect(source2.url).toContain('lastEventId=ep1%3A18294');

    client.disconnect();
  });

  it('clears lastEventId on reset_state and reconnects fresh', () => {
    const client = new RealtimeClient({ url: '/api/v1/events' });
    client.connect();
    const source1 = FakeEventSource.instances[0];
    source1.onopen?.();

    source1.emit('bank.transaction.credit', { transactionId: 'tx1' }, 'ep1:18294');
    expect(client.getLastEventId()).toBe('ep1:18294');

    // Emit reset_state (e.g. retention_expired)
    source1.emit('reset_state', { reason: 'retention_expired' });
    expect(client.getLastEventId()).toBeNull();
    expect(source1.closed).toBe(true);

    // Fast-forward reconnect timer
    vi.advanceTimersByTime(500);
    expect(FakeEventSource.instances).toHaveLength(2);
    const source2 = FakeEventSource.instances[1];
    expect(source2.url).not.toContain('lastEventId');

    client.disconnect();
  });

  it('transitions to STALE and reconnects when heartbeat watchdog times out', () => {
    let currentStatus = '';
    const client = new RealtimeClient({
      url: '/api/v1/events',
      staleThresholdMs: 5000,
      heartbeatCheckIntervalMs: 1000,
      onStatusChange: (s) => {
        currentStatus = s;
      },
    });

    client.connect();
    const source1 = FakeEventSource.instances[0];
    source1.onopen?.();
    expect(currentStatus).toBe('CONNECTED');

    // Advance time by 6s without heartbeat
    vi.advanceTimersByTime(6000);

    // Watchdog should mark STALE and trigger hardReconnect
    expect(source1.closed).toBe(true);
    expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(2);

    client.disconnect();
  });

  it('receives stream.heartbeat and updates lastHeartbeatAt', () => {
    const client = new RealtimeClient({ url: '/api/v1/events' });
    client.connect();
    const source = FakeEventSource.instances[0];
    source.onopen?.();

    const before = Date.now();
    source.emit('stream.heartbeat', {
      serverTime: new Date().toISOString(),
      epoch: 'ep1',
      release: 'rel-1',
      slot: 'blue',
    });

    expect(client.getLastHeartbeatAt()).toBeGreaterThanOrEqual(before);
    expect(client.getStatus()).toBe('CONNECTED');

    client.disconnect();
  });

  it('provides complete diagnostics snapshot', () => {
    let diagSnapshot: any = null;
    const client = new RealtimeClient({
      url: '/api/v1/events',
      onDiagnosticsChange: (d) => {
        diagSnapshot = d;
      },
    });

    client.connect();
    const source = FakeEventSource.instances[0];
    source.onopen?.();

    const diag = client.getDiagnostics();
    expect(diag.status).toBe('CONNECTED');
    expect(diag.serverReachable).toBe(true);
    expect(diag.reconnectCount).toBe(0);
    expect(diagSnapshot).not.toBeNull();
    expect(diagSnapshot.status).toBe('CONNECTED');

    client.disconnect();
  });

  it('reconnects when browser comes back online', () => {
    const listeners: Record<string, () => void> = {};
    vi.stubGlobal('window', {
      addEventListener: (evt: string, cb: () => void) => {
        listeners[evt] = cb;
      },
      removeEventListener: vi.fn(),
      location: { origin: 'http://localhost' },
    });

    const client = new RealtimeClient({ url: '/api/v1/events' });
    client.connect();
    expect(FakeEventSource.instances).toHaveLength(1);

    // Simulate offline
    listeners['offline']?.();
    expect(client.getStatus()).toBe('DISCONNECTED');
    expect(client.getDiagnostics().networkOnline).toBe(false);

    // Simulate online
    listeners['online']?.();
    expect(client.getStatus()).toBe('RECONNECTING');
    expect(client.getDiagnostics().networkOnline).toBe(true);
    expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(2);

    const source2 = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    source2.onopen?.();
    expect(client.getStatus()).toBe('CONNECTED');

    client.disconnect();
  });

  it('does not re-trigger reconnect during grace period when old heartbeat exists after reconnect', () => {
    const client = new RealtimeClient({
      url: '/api/v1/events',
      staleThresholdMs: 12000,
      heartbeatCheckIntervalMs: 2500,
    });

    client.connect();
    const source1 = FakeEventSource.instances[0];
    source1.onopen?.();

    // Receive first heartbeat
    source1.emit('stream.heartbeat', {
      serverTime: new Date().toISOString(),
      epoch: 'ep1',
    });
    expect(client.getLastHeartbeatAt()).not.toBeNull();

    // Advance system time by 60s (laptop sleep, timers frozen)
    vi.setSystemTime(Date.now() + 60000);

    // Wake / hard reconnect
    client.hardReconnect();
    expect(source1.closed).toBe(true);
    const source2 = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    source2.onopen?.();
    expect(client.getStatus()).toBe('CONNECTED');

    // Run watchdog after 3s (interval is 2500ms)
    vi.advanceTimersByTime(3000);

    // Source 2 must NOT be closed because it's within grace period of connectedAt
    expect(source2.closed).toBe(false);
    expect(client.getStatus()).toBe('CONNECTED');

    client.disconnect();
  });
});
