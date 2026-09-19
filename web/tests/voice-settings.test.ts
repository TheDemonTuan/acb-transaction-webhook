import { beforeEach, describe, expect, it } from 'vitest';
import {
  DEFAULT_VOICE_SETTINGS,
  loadVoiceSettings,
  saveVoiceSettings,
} from '../src/features/voice-announcements/voice-settings';
import { DEFAULT_ANNOUNCEMENT_TEMPLATE } from '../src/features/voice-announcements/voice-copy';

describe('voice-settings', () => {
  let store: Record<string, string> = {};

  beforeEach(() => {
    store = {};
    (globalThis as any).window = {
      localStorage: {
        getItem: (k: string) => store[k] ?? null,
        setItem: (k: string, v: string) => {
          store[k] = v;
        },
        removeItem: (k: string) => {
          delete store[k];
        },
      },
    };
  });

  it('loads default settings when empty', () => {
    const settings = loadVoiceSettings();
    expect(settings.enabled).toBe(false);
    expect(settings.volume).toBe(1);
    expect(settings.rate).toBe(1.25);
    expect(settings.includeDescription).toBe(false);
    expect(settings.announcementTemplate).toBe(DEFAULT_ANNOUNCEMENT_TEMPLATE);
  });

  it('saves and reloads modified settings including template', () => {
    saveVoiceSettings({
      enabled: true,
      volume: 0.8,
      rate: 1.5,
      includeDescription: true,
      announcementTemplate: 'Đã nhận {amount_raw} VND.',
    });
    const loaded = loadVoiceSettings();
    expect(loaded.enabled).toBe(true);
    expect(loaded.volume).toBe(0.8);
    expect(loaded.rate).toBe(1.5);
    expect(loaded.includeDescription).toBe(true);
    expect(loaded.announcementTemplate).toBe('Đã nhận {amount_raw} VND.');
  });

  it('clamps invalid values to 0.75x - 2.0x for rate', () => {
    saveVoiceSettings({ volume: 5, rate: 0.1 });
    const loaded = loadVoiceSettings();
    expect(loaded.volume).toBe(1); // clamped max 1
    expect(loaded.rate).toBe(0.75); // clamped min 0.75

    saveVoiceSettings({ rate: 3.5 });
    const loadedHigh = loadVoiceSettings();
    expect(loadedHigh.rate).toBe(2); // clamped max 2
  });

  it('restores default template when blank string provided', () => {
    saveVoiceSettings({ announcementTemplate: '   ' });
    const loaded = loadVoiceSettings();
    expect(loaded.announcementTemplate).toBe(DEFAULT_ANNOUNCEMENT_TEMPLATE);
  });
});
