import { afterEach, describe, expect, it } from 'bun:test';
import { markSpeechStarted } from '../src/features/voice-announcements/announcement-plan';
import type { VoiceMessage, VoiceTelemetry } from '../src/features/voice-announcements/voice-engine';
import { clearVoiceTelemetry, exportVoiceTelemetry, recordVoiceTelemetry } from '../src/features/voice-announcements/voice-telemetry';
describe('voice-telemetry', () => {
  afterEach(() => {
    clearVoiceTelemetry();
  });

  it('populates timing from SSE and queue to confirmed audio-clock playback', () => {
    const queuedAt = Date.now() - 20;
    const telemetry: VoiceTelemetry = {
      detectedAt: new Date(Date.now() - 1200).toISOString(),
      sseReceivedAt: Date.now() - 50,
      queuedAt,
      sseToQueueMs: 30,
      receivedMonotonicAt: 950,
    };
    let receivedTelemetry: VoiceTelemetry | undefined;
    const message: VoiceMessage = {
      text: 'Đã nhận mười nghìn đồng.',
      queuedMonotonicAt: 980,
      telemetry,
      onStart: (sample) => { receivedTelemetry = sample; },
    };

    markSpeechStarted(message, { monotonicNow: 1000, startEvidence: 'audio-clock',
      amountOffsetMs: 120, amountEvidence: 'reviewed-offset', audioPath: 'decoded-pack' });

    expect(receivedTelemetry).toBe(telemetry);
    expect(telemetry.speechStartedAt).toBeGreaterThanOrEqual(queuedAt);
    expect(telemetry.queueToSpeakMs).toBe(20);
    expect(telemetry.sseToSpeakMs).toBe(50);
    expect(telemetry.sseToAmountMs).toBe(170);
    expect(telemetry.startEvidence).toBe('audio-clock');
    expect(telemetry.amountEvidence).toBe('reviewed-offset');
    expect(telemetry.audioPath).toBe('decoded-pack');
  });

  it('records telemetry in bounded ring buffer without PII', () => {
    clearVoiceTelemetry();
    const payloadWithSecret = {
      detectedAt: '2026-10-11T12:00:00Z',
      sseToSpeakMs: 320,
      audioPath: 'decoded-pack' as const,
      outcome: 'completed' as const,
      text: 'Secret text should be omitted',
    };
    recordVoiceTelemetry(payloadWithSecret);

    const exported = exportVoiceTelemetry();
    expect(exported.schemaVersion).toBe(1);
    expect(exported.samples).toHaveLength(1);
    const firstSample = exported.samples[0];
    expect(firstSample.sseToSpeakMs).toBe(320);
    expect(firstSample.audioPath).toBe('decoded-pack');
    expect(firstSample.outcome).toBe('completed');
    expect('text' in firstSample).toBe(false);
  });
});
