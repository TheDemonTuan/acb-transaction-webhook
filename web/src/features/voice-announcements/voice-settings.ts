import { DEFAULT_ANNOUNCEMENT_TEMPLATE } from './voice-copy';

export interface VoiceSettings {
  enabled: boolean;
  voiceURI?: string;
  volume: number;
  rate: number;
  pitch: number;
  includeDescription: boolean;
  announceWhenHidden: boolean;
  burstMode: 'individual' | 'summary';
  announcementTemplate: string;
}

export const DEFAULT_VOICE_SETTINGS: VoiceSettings = {
  enabled: false,
  volume: 1,
  rate: 1.25,
  pitch: 1,
  includeDescription: false,
  announceWhenHidden: true,
  burstMode: 'summary',
  announcementTemplate: DEFAULT_ANNOUNCEMENT_TEMPLATE,
};

const STORAGE_KEY = 'acb.voice.settings.v1';

export function loadVoiceSettings(): VoiceSettings {
  if (typeof window === 'undefined' || !window.localStorage) {
    return { ...DEFAULT_VOICE_SETTINGS };
  }
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return { ...DEFAULT_VOICE_SETTINGS };
    const parsed = JSON.parse(raw);
    return {
      ...DEFAULT_VOICE_SETTINGS,
      ...parsed,
      // Clamp values (rate: 0.75x - 2.0x)
      volume: Math.min(1, Math.max(0, Number(parsed.volume ?? 1))),
      rate: Math.min(2, Math.max(0.75, Number(parsed.rate ?? 1.25))),
      pitch: Math.min(2, Math.max(0.5, Number(parsed.pitch ?? 1))),
      enabled: Boolean(parsed.enabled),
      includeDescription: Boolean(parsed.includeDescription),
      announceWhenHidden: Boolean(parsed.announceWhenHidden ?? true),
      burstMode: parsed.burstMode === 'individual' ? 'individual' : 'summary',
      announcementTemplate:
        typeof parsed.announcementTemplate === 'string' && parsed.announcementTemplate.trim() !== ''
          ? parsed.announcementTemplate
          : DEFAULT_ANNOUNCEMENT_TEMPLATE,
    };
  } catch {
    return { ...DEFAULT_VOICE_SETTINGS };
  }
}

export function saveVoiceSettings(settings: Partial<VoiceSettings>): VoiceSettings {
  const current = loadVoiceSettings();
  const next: VoiceSettings = {
    ...current,
    ...settings,
  };
  if (typeof window !== 'undefined' && window.localStorage) {
    try {
      window.localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
    } catch {}
  }
  return next;
}
