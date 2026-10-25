import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { TransactionAudioEngine } from './transaction-audio-engine';
import { MultiTabLeader } from './multi-tab-leader';
import { VoiceDedupe } from './voice-dedupe';
import type { VoiceEngine, VoiceMessage, VoiceTelemetry } from './voice-engine';
import { recordVoiceTelemetry } from './voice-telemetry';
import { VoiceQueue } from './voice-queue';
import { loadVoiceSettings, saveVoiceSettings, type VoiceSettings } from './voice-settings';
import { fetchVoiceProfile, type VoiceProfile } from './voice-profile';
import { fetchTransactionDetail } from '../../shared/api/queries';
import { isPublicViewerHost } from '../../app/runtime-mode';
import type { BankTransactionCreditData, RealtimeEnvelope } from '../../realtime/realtime.types';
import { creditTransactionKey } from '../../realtime/realtime.events';

export interface SkippedVoice { transactionId: string; reason: string }
export interface VoiceAnnouncementContextValue {
  settings: VoiceSettings;
  updateSettings: (changes: Partial<VoiceSettings>) => void;
  isSupported: boolean;
  isLeader: boolean;
  isSpeaking: boolean;
  audioUnlocked: boolean;
  profile?: VoiceProfile;
  profileOffline: boolean;
  voiceState: 'ready' | 'cold' | 'unavailable';
  voiceDetail?: string;
  pendingCount: number;
  skippedCount: number;
  recentSkipped: SkippedVoice[];
  lastTelemetry?: VoiceTelemetry | null;
  refreshProfile: (revision?: number) => void;
  handleCreditEvent: (envelope: RealtimeEnvelope<BankTransactionCreditData>) => void;
  testVoice: (customPhrase?: string) => Promise<void>;
  replayVoice: (transactionId: string) => Promise<void>;
  unlockAudio: () => Promise<boolean>;
  cancelVoice: () => void;
}
const VoiceAnnouncementContext = createContext<VoiceAnnouncementContextValue | null>(null);
export interface VoiceAnnouncementProviderProps { children: React.ReactNode; engine?: VoiceEngine }
interface VoiceOwner { engine: VoiceEngine; queue: VoiceQueue }

export const VoiceAnnouncementProvider: React.FC<VoiceAnnouncementProviderProps> = ({ children, engine: injectedEngine }) => {
  const queryClient = useQueryClient();
  const isPublic = isPublicViewerHost();
  const profileQuery = useQuery({ queryKey: ['voice-profile'], queryFn: () => fetchVoiceProfile(isPublic), retry: false,
    staleTime: Infinity, refetchOnWindowFocus: 'always', refetchOnReconnect: 'always' });
  const [settings, setSettings] = useState<VoiceSettings>(() => loadVoiceSettings());
  const [isLeader, setIsLeader] = useState(false);
  const [isSpeaking, setIsSpeaking] = useState(false);
  const [audioUnlocked, setAudioUnlocked] = useState(false);
  const [lastTelemetry, setLastTelemetry] = useState<VoiceTelemetry | null>(null);
  const [voiceState, setVoiceState] = useState<'ready' | 'cold' | 'unavailable'>('unavailable');
  const [voiceDetail, setVoiceDetail] = useState<string>();
  const [pendingCount, setPendingCount] = useState(0);
  const [skippedCount, setSkippedCount] = useState(0);
  const [recentSkipped, setRecentSkipped] = useState<SkippedVoice[]>([]);
  const [owner, setOwner] = useState<VoiceOwner | null>(null);
  const ownerRef = useRef<VoiceOwner | null>(null);
  const profileRef = useRef(profileQuery.data);
  profileRef.current = profileQuery.data;
  const requestedRevision = useRef(0);
  const unlocked = useRef(false);
  const dedupe = useMemo(() => new VoiceDedupe(), []);

  const refreshProfile = useCallback((revision?: number) => {
    if (revision !== undefined) {
      if (!Number.isSafeInteger(revision) || revision <= Math.max(profileRef.current?.revision || 0, requestedRevision.current)) return;
      requestedRevision.current = revision;
    }
    void queryClient.invalidateQueries({ queryKey: ['voice-profile'] });
  }, [queryClient]);

  useEffect(() => {
    let live = true;
    const engine = injectedEngine || new TransactionAudioEngine({ isPublic, onProfileChanged: () => refreshProfile(),
      onStatus: (state, detail) => {
        if (!live) return;
        if (detail === 'AUDIO_CONTEXT_NOT_RUNNING' || detail === 'CHIME_ASSET_UNAVAILABLE') {
          unlocked.current = false;
          setAudioUnlocked(false);
          setVoiceState('unavailable');
        } else setVoiceState(state);
        setVoiceDetail(detail);
      } });
    const queue = new VoiceQueue(engine, (state) => {
      if (live) { setIsSpeaking(state === 'speaking'); setPendingCount(queue.getPendingCount()); }
    }, { getProfile: () => profileRef.current });
    const next = { engine, queue };
    ownerRef.current = next; setOwner(next); unlocked.current = false;
    setAudioUnlocked(false);
    return () => {
      live = false; ownerRef.current = null; unlocked.current = false;
      // Injected engines have external ownership. Production owners are fresh on
      // each effect setup, including React StrictMode's setup-cleanup-setup cycle.
      if (injectedEngine) queue.cancel(); else queue.dispose();
    };
  }, [injectedEngine, isPublic, refreshProfile]);

  useEffect(() => {
    if (!owner || !profileQuery.data) return;
    if (owner.engine instanceof TransactionAudioEngine) owner.engine.setProfile(profileQuery.data);
    owner.queue.refreshPendingProfile();
    setVoiceState('cold');
  }, [owner, profileQuery.data]);

  useEffect(() => {
    const manager = new MultiTabLeader({ onLeaderChange: (leader) => {
      setIsLeader(leader);
      if (!leader) ownerRef.current?.queue.cancel();
    } });
    manager.start();
    return () => manager.stop();
  }, []);
  useEffect(() => {
    const storage = (event: StorageEvent) => {
      if (event.key !== 'acb.voice.settings.v1') return;
      const next = loadVoiceSettings(); setSettings(next);
      if (!next.enabled) ownerRef.current?.queue.cancel();
    };
    window.addEventListener('storage', storage);
    return () => window.removeEventListener('storage', storage);
  }, []);
  const updateSettings = useCallback((changes: Partial<VoiceSettings>) => {
    setSettings(saveVoiceSettings(changes));
    if (changes.enabled === false) ownerRef.current?.queue.cancel();
  }, []);
  const noteSkipped = useCallback((transactionId: string, reason: string) => {
    setSkippedCount((count) => count + 1);
    setRecentSkipped((items) => [{ transactionId, reason }, ...items].slice(0, 20));
  }, []);
  const handleCreditEvent = useCallback((envelope: RealtimeEnvelope<BankTransactionCreditData>) => {
    const active = ownerRef.current;
    if (!settings.enabled || settings.volume === 0 || !isLeader || !unlocked.current || !active || !profileRef.current) return;
    if (document.visibilityState === 'hidden' && !settings.announceWhenHidden) return;
    const data = envelope.data;
    if (!data || data.source !== 'REALTIME' || !/^[1-9][0-9]*$/.test(data.credit || '')) return;
    const amount = BigInt(data.credit);
    if (amount > 9007199254740991n) return;
    const keys = { eventId: envelope.id, transactionId: data.transactionId, semanticKey: creditTransactionKey(data) };
    if (dedupe.has(keys) || dedupe.isReserved(keys)) return;
    if (!dedupe.isFreshEnough(data.detectedAt, 30000, envelope.receivedAt)) {
      dedupe.markSkipped(keys, 'EXPIRED_FRESHNESS'); noteSkipped(data.transactionId, 'EXPIRED_FRESHNESS'); return;
    }
    if (!dedupe.reserve(keys)) return;
    const telemetry: VoiceTelemetry = { detectedAt: data.detectedAt, sseReceivedAt: envelope.receivedAt,
      receivedMonotonicAt: envelope.receivedMonotonicAt, queuedAt: Date.now(),
      sseToQueueMs: envelope.receivedMonotonicAt === undefined ? undefined : performance.now() - envelope.receivedMonotonicAt };
    const message: VoiceMessage = { text: '', amountVnd: amount.toString(), description: data.description,
      volume: settings.volume, transactionId: data.transactionId, chime: true, telemetry,
      onStart: (sample) => { if (sample) setLastTelemetry({ ...sample }); },
      onSuccess: () => {
        dedupe.commit(keys);
        setPendingCount(active.queue.getPendingCount());
        if (telemetry) {
          telemetry.outcome = 'completed';
          telemetry.speechEndedMonotonicAt = performance.now();
          recordVoiceTelemetry(telemetry);
          setLastTelemetry({ ...telemetry });
        }
      },
      onError: (error) => {
        const started = Boolean(error && typeof error === 'object' && 'playbackStarted' in error && (error as { playbackStarted?: unknown }).playbackStarted);
        const reason = error instanceof Error ? error.message : 'VOICE_UNAVAILABLE';
        if (started) dedupe.commit(keys);
        else if (reason === 'QUEUE_OVERFLOW' || reason === 'EXPIRED_AGE' || reason === 'VOICE_CANCELLED') dedupe.markSkipped(keys, reason);
        else dedupe.release(keys);
        noteSkipped(data.transactionId, reason); setPendingCount(active.queue.getPendingCount());
        if (telemetry) {
          telemetry.outcome = started
            ? 'interrupted'
            : reason === 'QUEUE_OVERFLOW'
            ? 'overflow'
            : reason === 'EXPIRED_AGE'
            ? 'expired'
            : reason === 'VOICE_CANCELLED'
            ? 'cancelled'
            : 'failed';
          recordVoiceTelemetry(telemetry);
          setLastTelemetry({ ...telemetry });
        }
      } };
    active.queue.enqueue(message); setPendingCount(active.queue.getPendingCount());
  }, [settings, isLeader, dedupe, noteSkipped]);

  const testVoice = useCallback(async (_customPhrase?: string) => {
    const active = ownerRef.current;
    if (!active || !profileRef.current) throw new Error('VOICE_PROFILE_UNAVAILABLE');
    if (settings.volume === 0) return;
    const telemetry: VoiceTelemetry = {
      isTest: true,
      queuedAt: Date.now(),
    };
    await active.queue.speak({
      text: '',
      amountVnd: '500000',
      isTest: true,
      volume: settings.volume,
      chime: true,
      telemetry,
      onStart: (sample) => { if (sample) setLastTelemetry({ ...sample }); },
      onSuccess: () => {
        telemetry.outcome = 'completed';
        telemetry.speechEndedMonotonicAt = performance.now();
        recordVoiceTelemetry(telemetry);
        setLastTelemetry({ ...telemetry });
      },
      onError: (error) => {
        const started = Boolean(error && typeof error === 'object' && 'playbackStarted' in error && (error as { playbackStarted?: unknown }).playbackStarted);
        telemetry.outcome = started ? 'interrupted' : 'failed';
        recordVoiceTelemetry(telemetry);
        setLastTelemetry({ ...telemetry });
      },
    });
  }, [settings.volume]);
  const replayVoice = useCallback(async (transactionId: string) => {
    const active = ownerRef.current;
    if (!active || !profileRef.current) throw new Error('VOICE_PROFILE_UNAVAILABLE');
    const generation = active.queue.getCancellationGeneration();
    const transaction = await fetchTransactionDetail(transactionId);
    if (ownerRef.current !== active || active.queue.getCancellationGeneration() !== generation) throw new Error('VOICE_CANCELLED');
    if (!Number.isSafeInteger(transaction.credit) || transaction.credit <= 0) throw new Error('VOICE_INVALID_AMOUNT');
    if (settings.volume === 0) return;
    const telemetry: VoiceTelemetry = {
      isReplay: true,
      queuedAt: Date.now(),
    };
    await active.queue.speak({
      text: '',
      amountVnd: String(transaction.credit),
      description: transaction.description,
      transactionId,
      isReplay: true,
      volume: settings.volume,
      telemetry,
      onStart: (sample) => { if (sample) setLastTelemetry({ ...sample }); },
      onSuccess: () => {
        telemetry.outcome = 'completed';
        telemetry.speechEndedMonotonicAt = performance.now();
        recordVoiceTelemetry(telemetry);
        setLastTelemetry({ ...telemetry });
      },
      onError: (error) => {
        const started = Boolean(error && typeof error === 'object' && 'playbackStarted' in error && (error as { playbackStarted?: unknown }).playbackStarted);
        telemetry.outcome = started ? 'interrupted' : 'failed';
        recordVoiceTelemetry(telemetry);
        setLastTelemetry({ ...telemetry });
      },
    });
  }, [settings.volume]);
  const unlockAudio = useCallback(async () => {
    const active = ownerRef.current;
    if (!active || !profileRef.current) return false;
    const engine = active.engine;
    let ready = true;
    if ('unlock' in engine && typeof engine.unlock === 'function') ready = await engine.unlock();
    if (ownerRef.current !== active) return false;
    unlocked.current = ready;
    setAudioUnlocked(ready);
    return ready;
  }, []);
  const cancelVoice = useCallback(() => { ownerRef.current?.queue.cancel(); setPendingCount(0); }, []);
  const value: VoiceAnnouncementContextValue = {
    settings, updateSettings, isSupported: owner?.engine.isSupported() ?? false, isLeader, isSpeaking, audioUnlocked,
    profile: profileQuery.data, profileOffline: profileQuery.isError, voiceState, voiceDetail, pendingCount, skippedCount,
    recentSkipped, lastTelemetry, refreshProfile, handleCreditEvent, testVoice, replayVoice, unlockAudio, cancelVoice,
  };
  return <VoiceAnnouncementContext.Provider value={value}>{children}</VoiceAnnouncementContext.Provider>;
};
export function useVoiceAnnouncements(): VoiceAnnouncementContextValue {
  const context = useContext(VoiceAnnouncementContext);
  if (!context) throw new Error('useVoiceAnnouncements must be used within a VoiceAnnouncementProvider');
  return context;
}
