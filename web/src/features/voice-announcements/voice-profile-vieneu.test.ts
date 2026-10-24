import { describe, expect, test } from 'bun:test';
import {
  DEFAULT_VOICE_PROFILE,
  validateVoiceProfile,
  synthesisProvider,
  VIENEU_VOICE_ID,
  type VoiceProfile,
} from './voice-profile';

describe('VieNeu-only voice profile', () => {
  test('synthesisProvider resolves the only supported provider', () => {
    expect(synthesisProvider('VIENEU_ONLY')).toBe('vieneu');
  });

  test('validates valid VIENEU_ONLY profile', () => {
    const raw: VoiceProfile = {
      ...DEFAULT_VOICE_PROFILE,
      providerMode: 'VIENEU_ONLY',
      voiceId: VIENEU_VOICE_ID,
      rate: 1.0,
      pitchHz: 0,
      synthesisKey: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    };
    const validated = validateVoiceProfile(raw);
    expect(validated.providerMode).toBe('VIENEU_ONLY');
    expect(validated.voiceId).toBe(VIENEU_VOICE_ID);
    expect(validated.rate).toBe(1.0);
    expect(validated.pitchHz).toBe(0);
  });

  test('rejects VIENEU_ONLY with rate != 1.0 or pitch != 0', () => {
    const invalidRate = {
      ...DEFAULT_VOICE_PROFILE,
      providerMode: 'VIENEU_ONLY',
      voiceId: VIENEU_VOICE_ID,
      rate: 1.25,
      pitchHz: 0,
      synthesisKey: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    };
    expect(() => validateVoiceProfile(invalidRate)).toThrow('VOICE_PROFILE_INVALID');

    const invalidPitch = {
      ...DEFAULT_VOICE_PROFILE,
      providerMode: 'VIENEU_ONLY',
      voiceId: VIENEU_VOICE_ID,
      rate: 1.0,
      pitchHz: 5,
      synthesisKey: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    };
    expect(() => validateVoiceProfile(invalidPitch)).toThrow('VOICE_PROFILE_INVALID');
  });
});
