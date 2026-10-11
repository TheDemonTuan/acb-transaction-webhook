import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  DEFAULT_VOICE_PROFILE, EDGE_VOICE_IDS, PIPER_VOICE_ID,
  fetchVoiceProfile, validateVoiceProfile,
} from '../src/features/voice-announcements/voice-profile';

const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64) };

describe('resolved server voice profile', () => {
  afterEach(() => { vi.unstubAllGlobals(); });

  it('defaults to the single self-hosted Piper voice at neutral pitch', () => {
    expect(validateVoiceProfile(profile)).toMatchObject({
      providerMode: 'PIPER_ONLY', voiceId: PIPER_VOICE_ID, pitchHz: 0,
    });
  });

  it.each([0.75, 1, 1.25, 2])('accepts Piper rate %s without altering the resolved profile', (rate) => {
    const resolved = { ...profile, rate };
    expect(validateVoiceProfile(resolved)).toBe(resolved);
  });

  it.each([
    { pitchHz: 1 }, { pitchHz: -1 }, { voiceId: EDGE_VOICE_IDS[0] },
    { voiceId: EDGE_VOICE_IDS[1] }, { voiceId: 'unpublished-piper-voice' },
    { rate: 0.74 }, { rate: 2.01 }, { rate: 1.251 },
  ])('rejects unsupported Piper settings %j', (override) => {
    expect(() => validateVoiceProfile({ ...profile, ...override })).toThrow('VOICE_PROFILE_INVALID');
  });

  it.each(EDGE_VOICE_IDS)('keeps %s explicitly selectable only in Edge mode', (voiceId) => {
    expect(validateVoiceProfile({ ...profile, providerMode: 'EDGE_ONLY', voiceId, pitchHz: 25 })).toMatchObject({ voiceId, pitchHz: 25 });
    expect(() => validateVoiceProfile({ ...profile, providerMode: 'EDGE_ONLY' })).toThrow('VOICE_PROFILE_INVALID');
  });

  it('keeps device voice selection separate in explicit browser mode', () => {
    expect(validateVoiceProfile({ ...profile, providerMode: 'BROWSER_ONLY', pitchHz: -25 })).toMatchObject({ providerMode: 'BROWSER_ONLY', pitchHz: -25 });
    expect(() => validateVoiceProfile({ ...profile, providerMode: 'ONLINE_AUTO' })).toThrow('VOICE_PROFILE_INVALID');
  });


  it('fetches the confirmed Piper profile without caching or changing its synthesis key', async () => {
    const fetcher = vi.fn(async () => new Response(JSON.stringify(profile)));
    vi.stubGlobal('fetch', fetcher);
    expect(await fetchVoiceProfile(true)).toEqual(profile);
    expect(fetcher).toHaveBeenCalledWith('/api/public/v1/voice/profile', expect.objectContaining({ cache: 'no-store' }));
  });
});
