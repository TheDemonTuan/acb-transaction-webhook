import { readFileSync } from 'node:fs';
import { expect, type Page } from '@playwright/test';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import type { VoiceManifest } from '../src/features/voice-announcements/audio-assets';
import type { VoiceTelemetry } from '../src/features/voice-announcements/voice-engine';

declare global {
  interface Window {
    __voiceSource?: EventSource;
    __voiceSignal: Array<{ at: number; rms: number }>;
    __voiceContexts: AudioContext[];
  }
}

export const SYNTHESIS_KEY = 'b5a2b7b8dec26c5ec21c3f8a7d91fa33ee75bcc587b51f444a6dbcb6bb34fcf0';
const assets = new URL('../../tts-gateway/assets/voicepacks/', import.meta.url);
export const manifest: VoiceManifest = JSON.parse(readFileSync(new URL(`${SYNTHESIS_KEY}/manifest.json`, assets), 'utf8'));
export const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: SYNTHESIS_KEY, funEnabled: false };
export const clipBytes = (id: string) => {
  const clip = manifest.clips.find(entry => entry.clipId === id);
  if (!clip) throw new Error(`Missing committed MP3: ${id}`);
  return readFileSync(new URL(`clips/${clip.sha256}.mp3`, assets));
};

export async function installVoiceFixture(page: Page, { enabled = false, cold = false } = {}) {
  const requests: string[] = [];
  await page.route(/\/api\/(?:public\/)?v1\/voice\/profile$/, route => route.fulfill({ json: profile }));
  await page.route(/\/api\/(?:public\/)?v1\/voice\/profiles\/[a-f0-9]+\/manifest$/, route =>
    route.fulfill(cold ? { status: 404, json: { error: 'Pack intentionally absent' } } : { json: manifest }));
  await page.route(/\/api\/(?:public\/)?v1\/voice\/assets\/[a-f0-9]+$/, route => {
    const hash = new URL(route.request().url()).pathname.split('/').at(-1);
    const clip = manifest.clips.find(entry => entry.sha256 === hash);
    if (!clip) return route.fulfill({ status: 404 });
    requests.push(`asset:${clip.clipId}`);
    return route.fulfill({ contentType: 'audio/mpeg', body: clipBytes(clip.clipId) });
  });
  await page.route(/\/api\/(?:public\/)?v1\/voice\/(?:test|transactions\/[^/]+(?:\/replay)?)\/stream(?:\?.*)?$/, route => {
    requests.push(route.request().url());
    return route.fulfill({ contentType: 'audio/mpeg', body: clipBytes('amount-500000'), headers: {
      'X-Voice-Revision': String(profile.revision), 'X-Voice-Synthesis-Key': profile.synthesisKey,
      'X-TTS-Voice': profile.voiceId, 'X-TTS-Provider': 'vieneu', 'X-TTS-Fallback': 'false',
    } });
  });
  await page.addInitScript(({ enabled }) => {
    localStorage.setItem('acb.voice.settings.v1', JSON.stringify({ schemaVersion: 2, enabled, volume: 1, announceWhenHidden: true }));
    const fixture = window;
    fixture.__voiceSignal = [];
    fixture.__voiceContexts = [];
    // Transport-only SSE fixture; native audio constructors and events remain untouched.
    const NativeEventSource = window.EventSource;
    window.EventSource = class extends NativeEventSource {
      constructor(url: string | URL, init?: EventSourceInit) { super(url, init); fixture.__voiceSource = this; }
    };
    // Tap the real destination graph without replacing audio nodes or their lifecycle.
    const connect = AudioNode.prototype.connect;
    AudioNode.prototype.connect = function (this: AudioNode, ...args: [AudioNode | AudioParam, number?, number?]) {
      const result = Reflect.apply(connect, this, args);
      if (args[0] === this.context.destination) {
        if (this.context instanceof AudioContext && !fixture.__voiceContexts.includes(this.context)) fixture.__voiceContexts.push(this.context);
        const analyser = this.context.createAnalyser();
        analyser.fftSize = 256;
        Reflect.apply(connect, this, [analyser]);
        const data = new Float32Array(analyser.fftSize);
        const sample = () => {
          if (this.context.state === 'closed') return;
          analyser.getFloatTimeDomainData(data);
          fixture.__voiceSignal.push({ at: performance.now(), rms: Math.sqrt(data.reduce((sum, x) => sum + x * x, 0) / data.length) });
          if (fixture.__voiceSignal.length > 20000) fixture.__voiceSignal.splice(0, 1000);
          requestAnimationFrame(sample);
        };
        requestAnimationFrame(sample);
      }
      return result;
    } as AudioNode['connect'];
  }, { enabled });
  return requests;
}

export async function telemetry(page: Page): Promise<VoiceTelemetry[]> {
  return page.evaluate(async () => {
    // Browser-realm module-boundary test: static Node imports read a different telemetry singleton.
    const modulePath = '/src/features/voice-announcements/voice-telemetry.ts';
    const module = await import(/* @vite-ignore */ modulePath);
    return module.exportVoiceTelemetry().samples;
  });
}

export async function expectCompleted(page: Page, count: number, path: VoiceTelemetry['audioPath'] = 'decoded-pack') {
  await expect.poll(async () => (await telemetry(page)).filter(sample => sample.outcome === 'completed').length,
    { timeout: 20000 }).toBe(count);
  const samples = await telemetry(page);
  expect(samples).toHaveLength(count);
  for (const sample of samples) {
    expect(sample.outcome).toBe('completed');
    expect(sample.audioPath).toBe(path);
    expect(sample.startEvidence).toBe(path === 'decoded-pack' ? 'audio-clock' : 'media-playing');
    expect(sample.speechEndedMonotonicAt!).toBeGreaterThan(sample.speechStartedMonotonicAt!);
  }
  return samples;
}

export async function emitCredit(page: Page, overrides: Record<string, unknown> = {}, eventId = 'voice:1') {
  await page.evaluate(({ overrides, eventId }) => {
    const source = window.__voiceSource;
    if (!source) throw new Error('SSE transport not initialized');
    const now = new Date().toISOString();
    source.dispatchEvent(new MessageEvent('bank.transaction.credit', { lastEventId: eventId, data: JSON.stringify({
      bank: 'KienlongBank', provider: 'PAYOS', transactionId: 'tx_e2e_realtime_500k', orderCode: '100000556677',
      transactionNumber: '556677', credit: '500000', debit: '0', currency: 'VND', source: 'REALTIME',
      transactionDate: now, detectedAt: now, description: 'FIRST REALTIME CREDIT', ...overrides,
    }) }));
  }, { overrides, eventId });
}

export async function unlockVoice(page: Page) {
  await page.getByRole('button', { name: 'Mở khóa loa', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Giọng đọc: Bật', exact: true })).toBeVisible();
}
