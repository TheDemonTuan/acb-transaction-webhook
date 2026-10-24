import { AudioAssets } from '../src/features/voice-announcements/audio-assets';
import { prepareAnnouncement, playPrepared, type PreparationOptions } from '../src/features/voice-announcements/announcement-plan';
import { RecentPhraseRing } from '../src/features/voice-announcements/voice-copy';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import { scheduleChime } from '../src/features/voice-announcements/transaction-audio-engine';
import { createSpeechOutput } from '../src/features/voice-announcements/speech-output';
import type { VoiceMessage, VoiceTelemetry } from '../src/features/voice-announcements/voice-engine';

const SAMPLE_RATE = 24000;
const CHIME_SAMPLE_RATE = 48000;
const profile = { ...DEFAULT_VOICE_PROFILE, funEnabled: false,
  synthesisKey: 'b5a2b7b8dec26c5ec21c3f8a7d91fa33ee75bcc587b51f444a6dbcb6bb34fcf0' };
function metrics(data: Float32Array) {
  requireEvidence(data.length > 0, 'Empty rendered sample window');
  let sum = 0, peak = 0, fullScaleSamples = 0;
  for (const value of data) {
    requireEvidence(Number.isFinite(value), 'Non-finite audio sample');
    sum += value * value;
    peak = Math.max(peak, Math.abs(value));
    if (Math.abs(value) >= 1) fullScaleSamples++;
  }
  return { rms: Math.sqrt(sum / data.length), peak, fullScaleSamples };
}
function requireEvidence(condition: boolean, message: string): asserts condition {
  if (!condition) throw new Error(message);
}
async function renderSpeech(buffer: AudioBuffer, volume: number) {
  const offline = new OfflineAudioContext(1, buffer.length, buffer.sampleRate);
  const output = createSpeechOutput(offline, volume);
  const source = offline.createBufferSource();
  source.buffer = buffer; source.connect(output.input); source.start();
  const rendered = await offline.startRendering();
  output.disconnect();
  return { ...metrics(rendered.getChannelData(0)), sampleCount: rendered.length,
    duration: rendered.duration, sampleRate: rendered.sampleRate };
}
async function renderChime(volume: number, baseline = false) {
  const offline = new OfflineAudioContext(1, CHIME_SAMPLE_RATE * 3, CHIME_SAMPLE_RATE);
  let start = 0, end = 0;
  if (baseline) {
    // Exact pre-change 250ms envelope; never used to replace production playback.
    for (const [frequency, offset, duration] of [[880, 0, 0.15], [1320, 0.10, 0.15]]) {
      const oscillator = offline.createOscillator();
      const gain = offline.createGain();
      oscillator.frequency.setValueAtTime(frequency, offset);
      gain.gain.setValueAtTime(0.0001, offset);
      gain.gain.exponentialRampToValueAtTime(0.075 * volume, offset + 0.01);
      gain.gain.exponentialRampToValueAtTime(0.02 * volume, offset + duration * 0.5);
      gain.gain.exponentialRampToValueAtTime(0.0001, offset + duration);
      oscillator.connect(gain); gain.connect(offline.destination);
      oscillator.start(offset); oscillator.stop(offset + duration);
    }
    end = 0.25;
    const rendered = await offline.startRendering();
    return { ...metrics(rendered.getChannelData(0).slice(0, end * CHIME_SAMPLE_RATE)),
      start, end, duration: end, sampleRate: rendered.sampleRate };
  }
  start = offline.currentTime;
  end = scheduleChime(offline, volume).end;
  const rendered = await offline.startRendering();
  const data = rendered.getChannelData(0);
  const from = Math.round(start * CHIME_SAMPLE_RATE), to = Math.round(end * CHIME_SAMPLE_RATE);
  const active = volume > 0 ? data.slice(from, to) : data;
  let lastActive = -1;
  for (let i = 0; i < data.length; i++) if (Math.abs(data[i]) > 0.000001) lastActive = i;
  return { ...metrics(active), start, end, duration: end - start,
    lastActiveSeconds: (lastActive + 1) / CHIME_SAMPLE_RATE, sampleRate: rendered.sampleRate };
}

async function offlineEvidence(context: AudioContext) {
  const assets = new AudioAssets(false);
  const signal = new AbortController().signal;
  const manifest = await assets.getManifest(profile, signal);
  requireEvidence(manifest !== null, 'Committed VieNeu manifest did not validate');
  const baseline = await renderChime(1, true);
  const chime = await renderChime(1);
  const chimeHalf = await renderChime(0.5);
  const chimeMuted = await renderChime(0);
  const chimeRatio = chime.rms / baseline.rms;
  requireEvidence(chime.sampleRate === 48000, 'Chime measurement must render at 48 kHz');
  requireEvidence(Math.abs(chime.duration - 1.2) <= 1 / CHIME_SAMPLE_RATE, 'Chime duration must be 1200ms');
  requireEvidence(Math.abs(chime.lastActiveSeconds! - chime.end) <= 1 / CHIME_SAMPLE_RATE, 'Chime must end at its scheduled sample');
  requireEvidence(Math.abs(chimeRatio - 4) <= 0.20, `Chime RMS ratio ${chimeRatio} is not 4x`);
  requireEvidence(chime.peak < 1, 'Chime must not clip');
  requireEvidence(Math.abs(chimeHalf.rms / chime.rms - 0.5) < 0.01, 'Chime device volume is not linear');
  requireEvidence(chimeMuted.peak === 0, 'Muted chime is not silent');

  const sine = context.createBuffer(1, SAMPLE_RATE, SAMPLE_RATE);
  for (let i = 0; i < sine.length; i++) sine.getChannelData(0)[i] = 0.1 * Math.sin(2 * Math.PI * 1000 * i / SAMPLE_RATE);
  const rawSine = metrics(sine.getChannelData(0));
  const speech = await renderSpeech(sine, 1);
  const speechHalf = await renderSpeech(sine, 0.5);
  const speechMuted = await renderSpeech(sine, 0);
  requireEvidence(Math.abs(speech.rms / rawSine.rms - 2.5) < 0.025, 'Unclipped speech must receive 2.5x gain');
  requireEvidence(Math.abs(speech.peak - 0.25) <= 0.0025, '1 kHz peak 0.10 speech sine must render at peak 0.25');
  requireEvidence(Math.abs(speechHalf.rms / rawSine.rms - 1.25) < 0.0125, 'Half-volume speech must receive 1.25x gain');
  requireEvidence(Math.abs(speechHalf.rms / speech.rms - 0.5) < 0.001, 'Speech volume is not linear');
  requireEvidence(speechMuted.peak === 0, 'Muted speech is not silent');
  const loudSine = context.createBuffer(1, SAMPLE_RATE, SAMPLE_RATE);
  for (let i = 0; i < loudSine.length; i++) loudSine.getChannelData(0)[i] = 0.8 * Math.sin(2 * Math.PI * 1000 * i / SAMPLE_RATE);
  const protectedSignal = await renderSpeech(loudSine, 1);
  const ceiling = 10 ** (-1 / 20);
  requireEvidence(protectedSignal.peak <= ceiling + 0.000001, 'Soft limiter exceeds -1 dBFS');
  requireEvidence(protectedSignal.peak <= 0.892 && protectedSignal.fullScaleSamples === 0,
    'Peak 0.8 loud sine must remain finite and below 0.892 without hard clipping');

  requireEvidence(manifest.clips.length === 140, 'Retained VieNeu pack must contain all 140 clips');
  const clips = [];
  for (const clip of manifest.clips) {
    const owner = {};
    const decoded = await assets.getDecoded(clip, profile, context, owner, signal);
    requireEvidence(decoded !== null, `Cannot decode committed clip ${clip.clipId}`);
    const raw = metrics(decoded.getChannelData(0));
    const processed = await renderSpeech(decoded, 1);
    requireEvidence(raw.rms > 0.00001 && processed.rms > raw.rms, `Silent or unamplified clip ${clip.clipId}`);
    requireEvidence(processed.peak <= ceiling + 0.000001, `Peak ceiling exceeded: ${clip.clipId}`);
    requireEvidence(processed.peak <= 0.892 && processed.fullScaleSamples === 0,
      `Clip must remain finite and below 0.892 without hard clipping: ${clip.clipId}`);
    requireEvidence(processed.sampleCount === decoded.length && processed.duration === decoded.duration &&
      processed.sampleRate === decoded.sampleRate, `Clip timing changed: ${clip.clipId}`);
    clips.push({ clipId: clip.clipId, raw, processed });
    assets.release(owner);
  }
  requireEvidence(assets.getStats().decodedBytes <= 16 * 1024 * 1024, 'Decoded cache exceeded 16 MiB');
  const samples = clips.filter(clip => ['amount-1000', 'amount-500000', 'amount-1000000', 'classic-001-500000'].includes(clip.clipId));
  assets.dispose();
  return { baseline, chime, chimeRatio, chimeHalf, chimeMuted, rawSine, speech, speechHalf, speechMuted,
    protectedSignal, clipCount: clips.length, samples, clips };
}

async function compressedEvidence(context: AudioContext) {
  const assets = new AudioAssets(false);
  const owner = {};
  const pin = context.createBuffer(1, 16 * 1024 * 1024 / 4, SAMPLE_RATE);
  requireEvidence(assets.retainDecoded('real-16-mib-pin', pin, profile.synthesisKey, owner) === pin, 'Cannot retain 16 MiB pin');
  const options: PreparationOptions = { isPublic: false, context, assets, recent: new RecentPhraseRing() };
  const signal = new AbortController().signal;
  const sample: VoiceTelemetry = {};
  const message: VoiceMessage = { text: '', isTest: true, amountVnd: '500000', volume: 0.5, telemetry: sample };
  const plan = await prepareAnnouncement(message, profile, signal, options);
  try {
    requireEvidence(plan.kind === 'stream' && plan.audioPath === 'compressed-pack', 'Pinned cache did not select real compressed-pack playback');
    requireEvidence(assets.getStats().decodedBytes === 16 * 1024 * 1024, 'Retained pin was evicted');
    const nativeEvents: string[] = [];
    for (const name of ['playing', 'timeupdate', 'ended']) plan.audioElement.addEventListener(name, event => {
      requireEvidence(event.isTrusted, `Synthetic media event: ${name}`);
      nativeEvents.push(name);
    });
    await playPrepared(plan, signal, options);
    requireEvidence(nativeEvents.includes('playing') && nativeEvents.includes('timeupdate') && nativeEvents.includes('ended'), 'Missing native media lifecycle');
    requireEvidence(sample.audioPath === 'compressed-pack' && sample.startEvidence === 'media-playing', 'Missing real media-start evidence');
    requireEvidence(plan.audioElement.currentTime > 0 && plan.audioElement.ended, 'MP3 did not actually finish');
    requireEvidence(plan.audioElement.volume === 1 && plan.volume === 0.5, 'Compressed playback must use shared graph volume, not native attenuation');
    const endedAt = performance.now();
    const activity = window.__voiceSignal.filter(frame => frame.at >= sample.speechStartedMonotonicAt! && frame.at <= endedAt);
    const maxRms = Math.max(0, ...activity.map(frame => frame.rms));
    requireEvidence(activity.length > 0 && maxRms > 0.0001, 'Compressed-pack media produced no real destination signal during playback');
    return { sample, nativeEvents, duration: plan.audioElement.duration, currentTime: plan.audioElement.currentTime,
      signal: { frames: activity.length, maxRms }, stats: assets.getStats() };
  } finally { plan.release(); assets.release(owner); assets.dispose(); }
}

document.querySelector<HTMLButtonElement>('#run')!.addEventListener('click', () => {
  const result = document.querySelector<HTMLOutputElement>('#result')!;
  const context = new AudioContext({ sampleRate: SAMPLE_RATE });
  const run = async () => {
    await context.resume();
    requireEvidence(context.state === 'running', 'Real audio context did not unlock');
    const offline = await offlineEvidence(context);
    const compressed = await compressedEvidence(context);
    console.info('VIENEU_REAL_AUDIO_RMS', JSON.stringify({ chimeRatio: offline.chimeRatio, samples: offline.samples }));
    result.value = JSON.stringify({ offline, compressed });
    result.dataset.status = 'completed';
  };
  void run().catch(error => { result.value = String(error); result.dataset.status = 'failed'; }).finally(() => context.close());
});
