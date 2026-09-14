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

  emit(type: string, data: unknown) {
    const event = { data: JSON.stringify(data), lastEventId: 'ep1:1' } as MessageEvent;
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

describe('RealtimeClient lifecycle', () => {
  beforeEach(() => {
    FakeEventSource.instances = [];
    vi.stubGlobal('window', {});
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  afterEach(() => {
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
  });

  it('does not open a duplicate source while connected', () => {
    const client = new RealtimeClient();

    client.connect();
    client.connect();

    expect(FakeEventSource.instances).toHaveLength(1);
  });
});
