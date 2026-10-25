import { vi } from 'bun:test';

export const CHIME_RECORDING_DURATION = 1.6554648526077098;
export const chimeRecordingBuffer = {
  duration: CHIME_RECORDING_DURATION, length: 73006, sampleRate: 44100, numberOfChannels: 1,
} as AudioBuffer;
const chimeBytes = new Uint8Array([82, 73, 70, 70, 4, 0, 0, 0, 87, 65, 86, 69]);

export function chimeRecordingResponse() {
  return new Response(chimeBytes.slice(), { headers: { 'Content-Type': 'audio/wav' } });
}

export class TestAudioNode {
  connect = vi.fn((target: unknown) => target);
  disconnect = vi.fn();
}

function parameter(value = 1) {
  return { value, setValueAtTime: vi.fn((_value: number, _time: number) => {}),
    exponentialRampToValueAtTime: vi.fn((_value: number, _time: number) => {}),
    linearRampToValueAtTime: vi.fn((_value: number, _time: number) => {}) };
}

class TestGainNode extends TestAudioNode { gain = parameter(); }
class TestWaveShaperNode extends TestAudioNode { curve: Float32Array | null = null; oversample = 'none'; }
class TestBufferSourceNode extends TestAudioNode {
  buffer: AudioBuffer | null = null;
  playbackRate = parameter();
  loop = false;
  detune = parameter(0);
  start = vi.fn((_time: number, _offset?: number, _duration?: number) => {});
  stop = vi.fn((_time?: number) => {}); onended: (() => void) | null = null;
}

export class TestAudioContext extends EventTarget {
  static owners: TestAudioContext[] = [];
  static initialState: AudioContextState = 'running';
  state: AudioContextState = TestAudioContext.initialState;
  currentTime = 10;
  destination = new TestAudioNode();
  gains: TestGainNode[] = [];
  shapers: TestWaveShaperNode[] = [];
  sources: TestBufferSourceNode[] = [];
  mediaSources: { audio: HTMLMediaElement; node: TestAudioNode }[] = [];
  resume = vi.fn(async () => this.setState('running'));
  close = vi.fn(async () => { this.state = 'closed'; });
  decodeAudioData = vi.fn(async (data: ArrayBuffer) => data.byteLength === chimeBytes.byteLength
    ? chimeRecordingBuffer : ({ duration: 1, length: 24000, numberOfChannels: 1 }) as AudioBuffer);
  constructor() { super(); TestAudioContext.owners.push(this); }
  setState(state: AudioContextState) { this.state = state; this.dispatchEvent(new Event('statechange')); }
  createGain() {
    const node = new TestGainNode(); this.gains.push(node); return node;
  }
  createWaveShaper() {
    const node = new TestWaveShaperNode(); this.shapers.push(node); return node;
  }
  createBufferSource() {
    const node = new TestBufferSourceNode(); this.sources.push(node); return node;
  }
  createMediaElementSource(audio: HTMLMediaElement) {
    const node = new TestAudioNode(); this.mediaSources.push({ audio, node }); return node;
  }
}

export class TestNativeAudio extends EventTarget {
  static owners: TestNativeAudio[] = [];
  static autoCanPlay = true;
  static autoPlaying = true;
  static autoEnded = true;
  static failPrepare = false;
  static playError: Error | null = null;
  static onLoaded: ((audio: TestNativeAudio) => void) | undefined;
  static onPlayed: ((audio: TestNativeAudio) => void) | undefined;
  src = '';
  volume = 1;
  playbackRate = 1;
  preload = '';
  pause = vi.fn();
  removeAttribute = vi.fn(() => { this.src = ''; });
  load = vi.fn(() => {
    if (!this.src) return;
    TestNativeAudio.onLoaded?.(this);
    if (TestNativeAudio.autoCanPlay) queueMicrotask(() => this.dispatchEvent(new Event(TestNativeAudio.failPrepare ? 'error' : 'canplay')));
  });
  play = vi.fn(async () => {
    if (TestNativeAudio.playError) throw TestNativeAudio.playError;
    if (TestNativeAudio.autoPlaying) {
      this.dispatchEvent(new Event('playing'));
      this.dispatchEvent(new Event('playing'));
    }
    TestNativeAudio.onPlayed?.(this);
    if (TestNativeAudio.autoEnded) queueMicrotask(() => this.dispatchEvent(new Event('ended')));
  });
  constructor() { super(); TestNativeAudio.owners.push(this); }
}

export function resetAudioTestFixture() {
  TestAudioContext.owners = []; TestAudioContext.initialState = 'running';
  TestNativeAudio.owners = []; TestNativeAudio.autoCanPlay = true;
  TestNativeAudio.autoPlaying = true; TestNativeAudio.autoEnded = true;
  TestNativeAudio.failPrepare = false; TestNativeAudio.playError = null;
  TestNativeAudio.onLoaded = undefined; TestNativeAudio.onPlayed = undefined;
}
