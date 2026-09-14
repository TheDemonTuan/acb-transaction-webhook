import React, { createContext, useContext, useEffect, useMemo, useState } from 'react';
import { TransactionAudioEngine } from './transaction-audio-engine';
import { MultiTabLeader } from './multi-tab-leader';
import { VoiceDedupe } from './voice-dedupe';
import type { VoiceEngine, VoiceInfo } from './voice-engine';
import { VoiceQueue } from './voice-queue';
import {
  loadVoiceSettings,
  saveVoiceSettings,
  type VoiceSettings,
} from './voice-settings';
import {
  buildSingleTransactionPhrase,
} from './voice-copy';
import type { BankTransactionCreditData, RealtimeEnvelope } from '../../realtime/realtime.types';

export interface VoiceAnnouncementContextValue {
  settings: VoiceSettings;
  updateSettings: (changes: Partial<VoiceSettings>) => void;
  isSupported: boolean;
  isLeader: boolean;
  isSpeaking: boolean;
  voices: VoiceInfo[];
  handleCreditEvent: (envelope: RealtimeEnvelope<BankTransactionCreditData>) => void;
  testVoice: (customPhrase?: string) => Promise<void>;
  replayVoice: (transactionId: string) => Promise<void>;
  unlockAudio: () => Promise<boolean>;
  cancelVoice: () => void;
}

const VoiceAnnouncementContext = createContext<VoiceAnnouncementContextValue | null>(null);

export interface VoiceAnnouncementProviderProps {
  children: React.ReactNode;
  engine?: VoiceEngine;
}

export const VoiceAnnouncementProvider: React.FC<VoiceAnnouncementProviderProps> = ({
  children,
  engine: injectedEngine,
}) => {
  const [settings, setSettings] = useState<VoiceSettings>(() => loadVoiceSettings());
  const [isLeader, setIsLeader] = useState(false);
  const [isSpeaking, setIsSpeaking] = useState(false);
  const [voices, setVoices] = useState<VoiceInfo[]>([]);

  const engine = useMemo<VoiceEngine>(() => {
    return injectedEngine || new TransactionAudioEngine();
  }, [injectedEngine]);

  const queue = useMemo(() => {
    return new VoiceQueue(engine, (state) => {
      setIsSpeaking(state === 'speaking');
    });
  }, [engine]);

  const dedupe = useMemo(() => new VoiceDedupe(), []);

  const leaderManager = useMemo(() => {
    return new MultiTabLeader({
      onLeaderChange: (leader) => setIsLeader(leader),
    });
  }, []);

  const isSupported = useMemo(() => engine.isSupported(), [engine]);

  // Load voices on mount and persistently listen for voiceschanged
  useEffect(() => {
    if (!isSupported) return;
    const refreshVoices = () => {
      engine.getVoices().then((v) => setVoices(v));
    };
    refreshVoices();
    if (typeof window !== 'undefined' && window.speechSynthesis) {
      window.speechSynthesis.addEventListener('voiceschanged', refreshVoices);
      return () => {
        window.speechSynthesis.removeEventListener('voiceschanged', refreshVoices);
      };
    }
  }, [engine, isSupported]);

  // Sync settings across tabs via localStorage storage event
  useEffect(() => {
    const handleStorage = (e: StorageEvent) => {
      if (e.key === 'acb.voice.settings.v1') {
        const fresh = loadVoiceSettings();
        setSettings(fresh);
        if (fresh.enabled === false) {
          queue.cancel();
        }
      }
    };
    if (typeof window !== 'undefined') {
      window.addEventListener('storage', handleStorage);
      return () => window.removeEventListener('storage', handleStorage);
    }
  }, [queue]);

  // Start leader manager on mount
  useEffect(() => {
    leaderManager.start();
    return () => {
      leaderManager.stop();
    };
  }, [leaderManager]);

  // Update settings handler
  const updateSettings = (changes: Partial<VoiceSettings>) => {
    const updated = saveVoiceSettings(changes);
    setSettings(updated);
    if (changes.enabled === false) {
      queue.cancel();
    }
  };

  const handleCreditEvent = (envelope: RealtimeEnvelope<BankTransactionCreditData>) => {
    if (!settings.enabled) return;
    if (!isLeader) return;

    // Check if browser tab is hidden and announceWhenHidden is false
    if (
      typeof document !== 'undefined' &&
      document.visibilityState === 'hidden' &&
      !settings.announceWhenHidden
    ) {
      return;
    }

    const data = envelope.data;
    if (!data) return;

    // Strict source policy: ONLY REALTIME sources are voice eligible
    if (data.source !== 'REALTIME') {
      return;
    }

    // Dedupe checks
    const dedupeOpts = {
      eventId: envelope.id,
      transactionId: data.transactionId,
      semanticKey: data.transactionNumber ? `ACB:${data.transactionNumber}` : undefined,
    };

    if (dedupe.has(dedupeOpts) || dedupe.isReserved(dedupeOpts)) {
      return;
    }

    // Freshness check: allow future clock skew up to 30s and max age 120s
    if (!dedupe.isFreshEnough(data.detectedAt, undefined, envelope.receivedAt)) {
      dedupe.markSkipped(dedupeOpts, 'EXPIRED_FRESHNESS');
      return;
    }

    // Parse amount
    let amountBigInt = 0n;
    try {
      const cleaned = (data.credit || '0').replace(/[^\d]/g, '');
      amountBigInt = BigInt(cleaned || '0');
    } catch {
      return;
    }

    if (amountBigInt <= 0n) return;

    // Two-phase reservation: reserve before enqueuing/playing
    if (!dedupe.reserve(dedupeOpts)) {
      return;
    }

    const text = buildSingleTransactionPhrase(amountBigInt.toString(), data.description || '', {
      includeDescription: settings.includeDescription,
    });
    queue.enqueue({
      text,
      volume: settings.volume,
      rate: settings.rate,
      pitch: settings.pitch,
      voiceURI: settings.voiceURI,
      transactionId: dedupeOpts.transactionId || undefined,
      includeDescription: settings.includeDescription,
      onSuccess: () => {
        dedupe.commit(dedupeOpts);
      },
      onError: () => {
        dedupe.release(dedupeOpts);
      },
    });
  };

  const testVoice = async (customPhrase?: string) => {
    queue.cancel();
    const testText =
      customPhrase || 'Đã bật đọc giao dịch mới. Bạn vừa nhận được năm trăm nghìn đồng.';

    return new Promise<void>((resolve, reject) => {
      queue.enqueue({
        text: testText,
        volume: settings.volume,
        rate: settings.rate,
        pitch: settings.pitch,
        voiceURI: settings.voiceURI,
        isTest: true,
        onSuccess: () => resolve(),
        onError: (err) => reject(err),
      });
    });
  };

  const replayVoice = async (transactionId: string) => {
    queue.cancel();
    return new Promise<void>((resolve, reject) => {
      queue.enqueue({
        text: 'Đang phát lại giao dịch...',
        volume: settings.volume,
        rate: settings.rate,
        pitch: settings.pitch,
        voiceURI: settings.voiceURI,
        transactionId,
        isReplay: true,
        includeDescription: settings.includeDescription,
        onSuccess: () => resolve(),
        onError: (err) => reject(err),
      });
    });
  };

  const unlockAudio = async (): Promise<boolean> => {
    if ('unlock' in engine && typeof (engine as any).unlock === 'function') {
      return await (engine as any).unlock();
    }
    return true;
  };

  const cancelVoice = () => {
    queue.cancel();
  };

  const value: VoiceAnnouncementContextValue = useMemo(() => {
    return {
      settings,
      updateSettings,
      isSupported,
      isLeader,
      isSpeaking,
      voices,
      handleCreditEvent,
      testVoice,
      replayVoice,
      unlockAudio,
      cancelVoice,
    };
  }, [settings, isSupported, isLeader, isSpeaking, voices]);

  return (
    <VoiceAnnouncementContext.Provider value={value}>
      {children}
    </VoiceAnnouncementContext.Provider>
  );
};

export function useVoiceAnnouncements(): VoiceAnnouncementContextValue {
  const ctx = useContext(VoiceAnnouncementContext);
  if (!ctx) {
    throw new Error('useVoiceAnnouncements must be used within a VoiceAnnouncementProvider');
  }
  return ctx;
}
