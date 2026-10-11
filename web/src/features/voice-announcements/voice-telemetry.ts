import type { VoiceTelemetry } from './voice-engine';

const MAX_RING_ENTRIES = 1000;
const ring: VoiceTelemetry[] = [];

/**
 * Strips any potential PII/text/audio bytes and records only telemetry metadata into ring.
 */
function sanitizeTelemetrySample(sample: Readonly<VoiceTelemetry>): VoiceTelemetry {
  return {
    detectedAt: sample.detectedAt,
    sseReceivedAt: sample.sseReceivedAt,
    queuedAt: sample.queuedAt,
    speechStartedAt: sample.speechStartedAt,
    sseToQueueMs: sample.sseToQueueMs,
    queueToSpeakMs: sample.queueToSpeakMs,
    sseToSpeakMs: sample.sseToSpeakMs,
    receivedMonotonicAt: sample.receivedMonotonicAt,
    speechStartedMonotonicAt: sample.speechStartedMonotonicAt,
    preparationStartedMonotonicAt: sample.preparationStartedMonotonicAt,
    audioReadyMonotonicAt: sample.audioReadyMonotonicAt,
    chimeStartedMonotonicAt: sample.chimeStartedMonotonicAt,
    chimeEndedMonotonicAt: sample.chimeEndedMonotonicAt,
    amountStartedMonotonicAt: sample.amountStartedMonotonicAt,
    speechEndedMonotonicAt: sample.speechEndedMonotonicAt,
    sseToChimeMs: sample.sseToChimeMs,
    sseToAmountMs: sample.sseToAmountMs,
    audioPath: sample.audioPath,
    startEvidence: sample.startEvidence,
    amountEvidence: sample.amountEvidence,
    outcome: sample.outcome,
    isTest: sample.isTest,
    isReplay: sample.isReplay,
  };
}

export function recordVoiceTelemetry(sample: Readonly<VoiceTelemetry>): void {
  const sanitized = sanitizeTelemetrySample(sample);
  if (ring.length >= MAX_RING_ENTRIES) {
    ring.shift();
  }
  ring.push(sanitized);
}

export function exportVoiceTelemetry(): { schemaVersion: 1; samples: VoiceTelemetry[] } {
  return {
    schemaVersion: 1,
    samples: ring.map((s) => ({ ...s })),
  };
}

export function clearVoiceTelemetry(): void {
  ring.length = 0;
}
