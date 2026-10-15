import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { setTestGlobal, restoreTestGlobals } from './test-globals';
import { DEFAULT_VOICE_SETTINGS, loadVoiceSettings, saveVoiceSettings } from '../src/features/voice-announcements/voice-settings';

describe('device voice preferences', () => {
  let store: Record<string, string>;
  beforeEach(() => {
    store = {};
    setTestGlobal('window', { localStorage: { getItem: (key: string) => store[key] ?? null,
      setItem: (key: string, value: string) => { store[key] = value; } } });
  });
  afterEach(() => restoreTestGlobals());
  it('defaults to disabled and never stores a server voice/rate/template locally', () => {
    expect(loadVoiceSettings()).toEqual(DEFAULT_VOICE_SETTINGS);
    saveVoiceSettings({ enabled: true, volume: 0.8, browserVoiceURI: 'selected-vi' });
    const loaded = loadVoiceSettings();
    expect(loaded).toMatchObject({ enabled: true, volume: 0.8, browserVoiceURI: 'selected-vi', schemaVersion: 2 });
    expect(loaded).not.toHaveProperty('rate');
    expect(loaded).not.toHaveProperty('voiceURI');
  });
  it('preserves mute volume zero and clamps bounds', () => {
    expect(saveVoiceSettings({ volume: 0 }).volume).toBe(0);
    expect(saveVoiceSettings({ volume: 5 }).volume).toBe(1);
    expect(saveVoiceSettings({ volume: -1 }).volume).toBe(0);
  });
  it('keeps a single legacy snapshot without applying or importing old config', () => {
    store['acb.voice.settings.v1'] = JSON.stringify({ enabled: true, volume: 0, announceWhenHidden: false,
      voiceURI: 'old-edge', rate: 2, pitch: 1.2, announcementTemplate: 'Nhận {amount}', includeDescription: true });
    const migrated = loadVoiceSettings();
    expect(migrated).toMatchObject({ schemaVersion: 2, enabled: true, volume: 0, announceWhenHidden: false,
      legacySnapshot: { voiceURI: 'old-edge', rate: 2, pitch: 1.2, announcementTemplate: 'Nhận {amount}', includeDescription: true } });
    expect(migrated.browserVoiceURI).toBeUndefined();
    expect(JSON.parse(store['acb.voice.settings.v1'])).not.toHaveProperty('rate');
    saveVoiceSettings({ legacySnapshot: undefined });
    expect(loadVoiceSettings().legacySnapshot).toBeUndefined();
  });
});
