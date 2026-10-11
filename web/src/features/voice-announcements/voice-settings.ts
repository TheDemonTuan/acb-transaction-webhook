export interface LegacyVoiceSnapshot {
  voiceURI?: string;
  rate?: number;
  pitch?: number;
  announcementTemplate?: string;
  includeDescription?: boolean;
}
export interface VoiceSettings {
  schemaVersion: 2;
  enabled: boolean;
  volume: number;
  announceWhenHidden: boolean;
  browserVoiceURI?: string;
  legacySnapshot?: LegacyVoiceSnapshot;
}
export const DEFAULT_VOICE_SETTINGS: VoiceSettings = {
  schemaVersion: 2, enabled: false, volume: 1, announceWhenHidden: true,
};
const STORAGE_KEY = 'acb.voice.settings.v1';
function legacySnapshot(value: unknown): LegacyVoiceSnapshot | undefined {
  if (!value || typeof value !== 'object') return;
  const snapshot: LegacyVoiceSnapshot = {};
  if ('voiceURI' in value && typeof value.voiceURI === 'string') snapshot.voiceURI = value.voiceURI;
  if ('rate' in value && typeof value.rate === 'number' && Number.isFinite(value.rate)) snapshot.rate = value.rate;
  if ('pitch' in value && typeof value.pitch === 'number' && Number.isFinite(value.pitch)) snapshot.pitch = value.pitch;
  if ('announcementTemplate' in value && typeof value.announcementTemplate === 'string') snapshot.announcementTemplate = value.announcementTemplate;
  if ('includeDescription' in value && typeof value.includeDescription === 'boolean') snapshot.includeDescription = value.includeDescription;
  return Object.keys(snapshot).length ? snapshot : undefined;
}
export function loadVoiceSettings(): VoiceSettings {
  if (typeof window === 'undefined') return { ...DEFAULT_VOICE_SETTINGS };
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return { ...DEFAULT_VOICE_SETTINGS };
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object') return { ...DEFAULT_VOICE_SETTINGS };
    const currentSchema = 'schemaVersion' in parsed && parsed.schemaVersion === 2;
    const volume = 'volume' in parsed && typeof parsed.volume === 'number' && Number.isFinite(parsed.volume) ? parsed.volume : 1;
    const migrated: VoiceSettings = {
      schemaVersion: 2,
      enabled: 'enabled' in parsed && parsed.enabled === true,
      volume: Math.max(0, Math.min(1, volume)),
      announceWhenHidden: !('announceWhenHidden' in parsed) || parsed.announceWhenHidden !== false,
      browserVoiceURI: currentSchema && 'browserVoiceURI' in parsed && typeof parsed.browserVoiceURI === 'string' ? parsed.browserVoiceURI : undefined,
      legacySnapshot: currentSchema ? legacySnapshot('legacySnapshot' in parsed ? parsed.legacySnapshot : undefined) : legacySnapshot(parsed),
    };
    if (!currentSchema) window.localStorage.setItem(STORAGE_KEY, JSON.stringify(migrated));
    return migrated;
  } catch { return { ...DEFAULT_VOICE_SETTINGS }; }
}
export function saveVoiceSettings(changes: Partial<VoiceSettings>): VoiceSettings {
  const current = loadVoiceSettings();
  const volume = changes.volume ?? current.volume;
  const next: VoiceSettings = {
    schemaVersion: 2, enabled: changes.enabled ?? current.enabled,
    volume: Number.isFinite(volume) ? Math.max(0, Math.min(1, volume)) : current.volume,
    announceWhenHidden: changes.announceWhenHidden ?? current.announceWhenHidden,
    browserVoiceURI: 'browserVoiceURI' in changes ? changes.browserVoiceURI : current.browserVoiceURI,
    legacySnapshot: 'legacySnapshot' in changes ? changes.legacySnapshot : current.legacySnapshot,
  };
  try { if (typeof window !== 'undefined') window.localStorage.setItem(STORAGE_KEY, JSON.stringify(next)); } catch {}
  return next;
}
