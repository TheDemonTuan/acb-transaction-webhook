import type { VoiceProfile } from './voice-profile';
import type { AnnouncementRecipe } from './voice-copy';

export interface VoiceInfo {
  uri: string;
  name: string;
  lang: string;
  isDefault: boolean;
}

export interface VoiceTelemetry {
  detectedAt?: string;
  sseReceivedAt?: number;
  queuedAt?: number;
  speechStartedAt?: number;
  detectedToSseMs?: number;
  sseToQueueMs?: number;
  queueToSpeakMs?: number;
  sseToSpeakMs?: number;
  totalLatencyMs?: number;
  receivedMonotonicAt?: number;
  speechStartedMonotonicAt?: number;
}

export interface VoiceMessage {
  text: string;
  volume?: number;
  voiceURI?: string;
  lang?: string;
  transactionId?: string;
  summaryTransactionIds?: string[];
  isTest?: boolean;
  isReplay?: boolean;
  phraseId?: string;
  amountVnd?: string;
  omitFun?: boolean;
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
  getVoices(): Promise<VoiceInfo[]>;
  speak(message: VoiceMessage): Promise<void>;
  prefetch(message: VoiceMessage, signal: AbortSignal): Promise<void>;
  cancel(): void;
  dispose(): void | Promise<void>;
}

