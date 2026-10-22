import { afterEach, describe, expect, it, vi } from 'bun:test';
import { setTestGlobal, restoreTestGlobals } from './test-globals';
import {
  DEFAULT_VOICE_PROFILE, EDGE_VOICE_IDS, PIPER_VOICE_ID,
  fetchVoiceProfile, validateVoiceProfile,
  type VoiceProviderMode,
} from '../src/features/voice-announcements/voice-profile';

const piperProfile = { ...DEFAULT_VOICE_PROFILE, providerMode: 'PIPER_ONLY' as const, voiceId: PIPER_VOICE_ID, rate: 1.25, synthesisKey: 'a'.repeat(64) };
const defaultProfile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64) };

describe('resolved server voice profile', () => {
  afterEach(() => { restoreTestGlobals(); });

  it('defaults to the single self-hosted VieNeu voice at neutral pitch and 1.0x', () => {
    expect(validateVoiceProfile(defaultProfile)).toMatchObject({
      providerMode: 'VIENEU_ONLY', voiceId: 'vi-VN-vieneu-v3-turbo-mai-anh-v1', pitchHz: 0, rate: 1.0,
    });
  });

  it.each([0.75, 1, 1.25, 2])('accepts Piper rate %s without altering the resolved profile', (rate) => {
    const resolved = { ...piperProfile, rate };
    expect(validateVoiceProfile(resolved)).toBe(resolved);
  });

  it.each([
    { pitchHz: 1 }, { pitchHz: -1 }, { voiceId: EDGE_VOICE_IDS[0] },
    { voiceId: EDGE_VOICE_IDS[1] }, { voiceId: 'unpublished-piper-voice' },
    { rate: 0.74 }, { rate: 2.01 }, { rate: 1.251 },
  ])('rejects unsupported Piper settings %j', (override) => {
    expect(() => validateVoiceProfile({ ...piperProfile, ...override })).toThrow('VOICE_PROFILE_INVALID');
  });

  it.each(EDGE_VOICE_IDS)('keeps %s explicitly selectable only in Edge mode', (voiceId) => {
    expect(validateVoiceProfile({ ...defaultProfile, providerMode: 'EDGE_ONLY', voiceId, pitchHz: 25 })).toMatchObject({ voiceId, pitchHz: 25 });
    expect(() => validateVoiceProfile({ ...defaultProfile, providerMode: 'EDGE_ONLY', voiceId: PIPER_VOICE_ID })).toThrow('VOICE_PROFILE_INVALID');
  });

  it('keeps device voice selection separate in explicit browser mode', () => {
    expect(validateVoiceProfile({ ...defaultProfile, providerMode: 'BROWSER_ONLY', pitchHz: -25 })).toMatchObject({ providerMode: 'BROWSER_ONLY', pitchHz: -25 });
    expect(() => validateVoiceProfile({ ...defaultProfile, providerMode: 'ONLINE_AUTO' as unknown as VoiceProviderMode })).toThrow('VOICE_PROFILE_INVALID');
  });


  it('fetches the confirmed profile without caching or changing its synthesis key', async () => {
    const fetcher = vi.fn(async () => new Response(JSON.stringify(defaultProfile)));
    setTestGlobal('fetch', fetcher);
    expect(await fetchVoiceProfile(true)).toEqual(defaultProfile);
    expect(fetcher).toHaveBeenCalledWith('/api/public/v1/voice/profile', expect.objectContaining({ cache: 'no-store' }));
  });
});
