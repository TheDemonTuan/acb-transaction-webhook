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
}

export interface VoiceMessage {
  text: string;
  volume?: number;
  rate?: number;
  pitch?: number;
  voiceURI?: string;
  lang?: string;
  transactionId?: string;
  summaryTransactionIds?: string[];
  isTest?: boolean;
  isReplay?: boolean;
  includeDescription?: boolean;
  template?: string;
  telemetry?: VoiceTelemetry;
  onStart?: (telemetry?: VoiceTelemetry) => void;
  onSuccess?: () => void;
  onError?: (error: unknown) => void;
}

export interface VoiceEngine {
  isSupported(): boolean;
  getVoices(): Promise<VoiceInfo[]>;
  speak(message: VoiceMessage): Promise<void>;
  cancel(): void;
}

