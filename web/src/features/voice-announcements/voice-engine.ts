import type { VoiceProfile } from './voice-profile';
import type { AnnouncementRecipe } from './voice-copy';

export interface VoiceTelemetry {
  detectedAt?: string;
  sseReceivedAt?: number;
  queuedAt?: number;
  speechStartedAt?: number;
  sseToQueueMs?: number;
  queueToSpeakMs?: number;
  sseToSpeakMs?: number;
  receivedMonotonicAt?: number;
  speechStartedMonotonicAt?: number;
  preparationStartedMonotonicAt?: number;
  audioReadyMonotonicAt?: number;
  chimeStartedMonotonicAt?: number;
  chimeEndedMonotonicAt?: number;
  amountStartedMonotonicAt?: number;
  speechEndedMonotonicAt?: number;
  sseToChimeMs?: number;
  sseToAmountMs?: number;
  audioPath?: 'decoded-pack' | 'compressed-pack' | 'network-pack' | 'cold-stream' | 'cold-buffer';
  startEvidence?: 'audio-clock' | 'media-playing';
  amountEvidence?: 'reviewed-offset' | 'segment-boundary' | 'unavailable';
  outcome?: 'completed' | 'failed' | 'interrupted' | 'overflow' | 'expired' | 'cancelled';
  isTest?: boolean;
  isReplay?: boolean;
}

export interface VoiceMessage {
  text: string;
  volume?: number;
  transactionId?: string;
  summaryTransactionIds?: string[];
  isTest?: boolean;
  isReplay?: boolean;
  phraseId?: string;
  amountVnd?: string;
  profile?: Readonly<VoiceProfile>;
  recipe?: AnnouncementRecipe;
  description?: string;
  chime?: boolean;
  onTailStart?: (phraseId: string) => void;
  queuedMonotonicAt?: number;
  telemetry?: VoiceTelemetry;
  onStart?: (telemetry?: VoiceTelemetry) => void;
  onSuccess?: () => void;
  onError?: (error: unknown) => void;
}

export interface VoiceEngine {
  isSupported(): boolean;
  speak(message: VoiceMessage): Promise<void>;
  prefetch(message: VoiceMessage, signal: AbortSignal): Promise<void>;
  cancel(): void;
  dispose(): void | Promise<void>;
}

export function isVoiceCancellation(error: unknown): boolean {
  return error instanceof Error && error.message === 'VOICE_CANCELLED';
}

