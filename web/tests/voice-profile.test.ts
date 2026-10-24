import { afterEach, describe, expect, it, vi } from 'bun:test';
import { setTestGlobal, restoreTestGlobals } from './test-globals';
import {
  DEFAULT_VOICE_PROFILE, VIENEU_VOICE_ID, VIENEU_VOICE_LABEL,
  fetchVoiceProfile, saveVoiceSettingsServer, validateVoiceProfile,
} from '../src/features/voice-announcements/voice-profile';

const defaultProfile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64) };

describe('resolved server voice profile', () => {
  afterEach(() => { restoreTestGlobals(); });

  it('defaults to the single self-hosted VieNeu voice at neutral pitch and 1.0x', () => {
    expect(validateVoiceProfile(defaultProfile)).toMatchObject({
      providerMode: 'VIENEU_ONLY', voiceId: 'vi-VN-vieneu-v3-turbo-mai-anh-v1', pitchHz: 0, rate: 1.0,
    });
  });

  it.each([
    { providerMode: 'PIPER_ONLY' }, { providerMode: 'EDGE_ONLY' },
    { providerMode: 'BROWSER_ONLY' }, { providerMode: 'ONLINE_AUTO' },
    { pitchHz: 1 }, { pitchHz: -1 }, { voiceId: 'vi-VN-HoaiMyNeural' },
    { voiceId: 'vi-VN-NamMinhNeural' }, { voiceId: 'vi-VN-vais1000-medium' },
    { voiceId: '' }, { rate: 0.75 }, { rate: 1.25 }, { rate: 2 }, { rate: Number.NaN },
  ])('rejects any non-VieNeu or non-neutral profile %j', (override) => {
    expect(() => validateVoiceProfile({ ...defaultProfile, ...override })).toThrow('VOICE_PROFILE_INVALID');
  });

  it('publishes the fixed voice identity', () => {
    expect(VIENEU_VOICE_ID).toBe('vi-VN-vieneu-v3-turbo-mai-anh-v1');
    expect(VIENEU_VOICE_LABEL).toBe('VieNeu · Mai Anh · nữ miền Bắc');
  });


  it('fetches the confirmed profile without caching or changing its synthesis key', async () => {
    const fetcher = vi.fn(async () => new Response(JSON.stringify(defaultProfile)));
    setTestGlobal('fetch', fetcher);
    expect(await fetchVoiceProfile(true)).toEqual(defaultProfile);
    expect(fetcher).toHaveBeenCalledWith('/api/public/v1/voice/profile', expect.objectContaining({ cache: 'no-store' }));
  });

  it('preserves authenticated revision-CAS saves and surfaces conflicts', async () => {
    const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
      if (url.endsWith('/csrf')) return new Response(JSON.stringify({ token: 'voice-csrf' }));
      expect(url).toBe('/api/v1/voice/settings');
      expect(init?.credentials).toBe('same-origin');
      expect(init?.method).toBe('PUT');
      expect(new Headers(init?.headers).has('X-CSRF-Token')).toBe(true);
      expect(JSON.parse(String(init?.body))).toMatchObject({ revision: defaultProfile.revision,
        providerMode: 'VIENEU_ONLY', voiceId: VIENEU_VOICE_ID, rate: 1, pitchHz: 0 });
      return new Response(JSON.stringify({ error: 'conflict' }), {
        status: 409, headers: { 'Content-Type': 'application/json' },
      });
    });
    setTestGlobal('fetch', fetcher);
    await expect(saveVoiceSettingsServer(defaultProfile)).rejects.toThrow('VOICE_SETTINGS_CONFLICT');
  });
});
