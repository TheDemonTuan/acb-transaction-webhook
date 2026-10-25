import { AudioAssets } from '../src/features/voice-announcements/audio-assets';
import { prepareAnnouncement, playPrepared, type PreparationOptions } from '../src/features/voice-announcements/announcement-plan';
import { RecentPhraseRing } from '../src/features/voice-announcements/voice-copy';
import { DEFAULT_VOICE_PROFILE } from '../src/features/voice-announcements/voice-profile';
import { scheduleChime } from '../src/features/voice-announcements/transaction-audio-engine';
import { loadChimeRecording } from '../src/features/voice-announcements/chime-recording';
import { createSpeechOutput } from '../src/features/voice-announcements/speech-output';
import type { VoiceMessage, VoiceTelemetry } from '../src/features/voice-announcements/voice-engine';

const SAMPLE_RATE = 24000;
const CHIME_SAMPLE_RATE = 44100;
const CHIME_FRAME_COUNT = 73006;
const CHIME_DURATION = 1.6554648526077098;
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
function recordingMetrics(buffer: AudioBuffer, from = 0, to = buffer.length) {
  let energy = 0, peak = 0, fullScaleSamples = 0, firstActiveSample = -1, lastActiveSample = -1;
  for (let channel = 0; channel < buffer.numberOfChannels; channel++) {
    const data = buffer.getChannelData(channel).subarray(from, to);
    const measured = metrics(data);
    energy += measured.rms ** 2 * data.length;
    peak = Math.max(peak, measured.peak);
    fullScaleSamples += measured.fullScaleSamples;
    for (let i = 0; i < data.length; i++) {
      if (data[i] === 0) continue;
      if (firstActiveSample === -1 || i < firstActiveSample) firstActiveSample = i;
      lastActiveSample = Math.max(lastActiveSample, i);
    }
  }
  return { rms: Math.sqrt(energy / ((to - from) * buffer.numberOfChannels)), peak, energy, fullScaleSamples,
    firstActiveSample, lastActiveSample,
    activeDuration: firstActiveSample === -1 ? 0 : (lastActiveSample - firstActiveSample + 1) / buffer.sampleRate };
}
async function renderChime(buffer: AudioBuffer, volume: number) {
  const offline = new OfflineAudioContext(buffer.numberOfChannels,
    buffer.length + Math.ceil(buffer.sampleRate * 0.25), buffer.sampleRate);
  const start = offline.currentTime;
  const level = Math.max(0, Math.min(1, volume));
  const { end, nodes } = scheduleChime(offline, volume, buffer);
  if (level === 0) {
    requireEvidence(nodes.length === 0 && end === 0, 'Muted recording must not create nodes or delay speech');
  } else {
    requireEvidence(nodes.length === 1, 'Recording must use exactly one source and gain');
    const { source } = nodes[0];
    requireEvidence(source.buffer === buffer && source.playbackRate.value === 1 && source.detune.value === 0 && !source.loop,
      'Recording must play once at its original rate without substitution or looping');
  }
  const rendered = await offline.startRendering();
  requireEvidence(rendered.numberOfChannels === buffer.numberOfChannels, 'Recording channel count changed');
  let sampleDifferencePeak = 0;
  for (let channel = 0; channel < buffer.numberOfChannels; channel++) {
    const original = buffer.getChannelData(channel), actual = rendered.getChannelData(channel);
    for (let i = 0; i < original.length; i++) {
      sampleDifferencePeak = Math.max(sampleDifferencePeak, Math.abs(actual[i] - original[i] * 4 * level));
    }
  }
  requireEvidence(sampleDifferencePeak <= 0.0000001, 'Playback changed the recorded waveform beyond 4× clamped volume scaling');
  const measured = recordingMetrics(rendered, 0, buffer.length);
  const postEnd = recordingMetrics(rendered, buffer.length);
  requireEvidence(measured.peak < 1 && measured.fullScaleSamples === 0, 'Recording playback must have no full-scale samples');
  if (level > 0) requireEvidence(Math.abs(end - start - buffer.duration) <= 1 / buffer.sampleRate,
    'Scheduled end must equal the complete encoded recording duration');
  requireEvidence(postEnd.peak === 0 && postEnd.energy === 0, 'Recording must be silent after its encoded end');
  return { ...measured, start, end, duration: end - start, encodedFrames: buffer.length,
    sampleDifferencePeak, postEnd, sampleRate: rendered.sampleRate, channels: rendered.numberOfChannels };
}

async function offlineEvidence(context: AudioContext) {
  const assets = new AudioAssets(false);
  const signal = new AbortController().signal;
  const manifest = await assets.getManifest(profile, signal);
  requireEvidence(manifest !== null, 'Committed VieNeu manifest did not validate');
  // Decode the production WAV at its native sample rate; never reconstruct its waveform in the harness.
  const decodingContext = new OfflineAudioContext(1, 1, CHIME_SAMPLE_RATE);
  const recording = await loadChimeRecording(decodingContext, signal);
  requireEvidence(recording.sampleRate === CHIME_SAMPLE_RATE && recording.length === CHIME_FRAME_COUNT &&
    recording.numberOfChannels === 1, 'Bundled Calm.wav encoded format changed');
  requireEvidence(Math.abs(recording.duration - CHIME_DURATION) <= 1 / CHIME_SAMPLE_RATE,
    'Bundled Calm.wav encoded duration changed');
  const recorded = { ...recordingMetrics(recording), duration: recording.duration,
    sampleCount: recording.length, sampleRate: recording.sampleRate, channels: recording.numberOfChannels };
  requireEvidence(recorded.peak > 0 && recorded.peak < 1 && recorded.fullScaleSamples === 0,
    'Calm recording must be audible with headroom and no full-scale samples');
  requireEvidence(recorded.energy > 0 && recorded.activeDuration >= 1.2 && recorded.activeDuration <= recorded.duration,
    'Calm recording must contain at least 1.2 seconds of active audio within its encoded duration');
  const chime = await renderChime(recording, 1);
  const chimeHalf = await renderChime(recording, 0.5);
  const chimeMuted = await renderChime(recording, 0);
  requireEvidence(chime.firstActiveSample === recorded.firstActiveSample && chime.lastActiveSample === recorded.lastActiveSample &&
    chime.activeDuration === recorded.activeDuration, 'Playback changed the recording active-audio extent');
  requireEvidence(chime.peak < 1 && chime.fullScaleSamples === 0 && chime.energy > 0,
    'Recording playback must retain energy and amplitude headroom');
  requireEvidence(Math.abs(chime.rms / recorded.rms - 4) < 0.000001 &&
    Math.abs(chime.peak / recorded.peak - 4) < 0.000001, 'Full-volume recording must use 4× amplitude gain');
  requireEvidence(Math.abs(chimeHalf.rms / chime.rms - 0.5) < 0.000001 &&
    Math.abs(chimeHalf.peak / chime.peak - 0.5) < 0.000001 &&
    Math.abs(chimeHalf.energy / chime.energy - 0.25) < 0.000001, 'Recording volume and energy scaling changed');
  requireEvidence(chimeHalf.firstActiveSample === recorded.firstActiveSample &&
    chimeHalf.lastActiveSample === recorded.lastActiveSample, 'Half-volume playback changed the recording active duration');
  requireEvidence(chimeMuted.peak === 0 && chimeMuted.energy === 0 && chimeMuted.activeDuration === 0,
    'Muted recording must contain no signal or active samples');

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
  return { recorded, chime, chimeHalf, chimeMuted, rawSine, speech, speechHalf, speechMuted,
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
    console.info('VIENEU_REAL_AUDIO_RMS', JSON.stringify({ chime: offline.chime, samples: offline.samples }));
    result.value = JSON.stringify({ offline, compressed });
    result.dataset.status = 'completed';
  };
  void run().catch(error => { result.value = String(error); result.dataset.status = 'failed'; }).finally(() => context.close());
});
