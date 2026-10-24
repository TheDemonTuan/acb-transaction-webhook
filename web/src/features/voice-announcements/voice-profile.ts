import { api } from '../../api';

export interface AmountBand {
  id: 'small' | 'medium' | 'large' | 'celebrate';
  fromVnd: string;
  toVnd: string | null;
  styleId: 'classic' | 'cute' | 'funny' | 'custom';
}

export type VoiceProviderMode = 'VIENEU_ONLY';
export const VIENEU_VOICE_ID = 'vi-VN-vieneu-v3-turbo-mai-anh-v1';
export const VIENEU_VOICE_LABEL = 'VieNeu · Mai Anh · nữ miền Bắc';

export function synthesisProvider(_mode: VoiceProviderMode): 'vieneu' {
  return 'vieneu';
}
export interface VoiceProfile {
  revision: number;
  providerMode: VoiceProviderMode;
  voiceId: string;
  rate: number;
  pitchHz: number;
  amountBands: AmountBand[];
  chimeEnabled: boolean;
  loadingSoundEnabled: boolean;
  funEnabled: boolean;
  includeDescription: boolean;
  customTemplate: string;
  catalogVersion: 'v1';
  synthesisKey: string;
  coldRequestTimeoutMs: number;
  updatedAt: string;
}

export interface VoiceSettingsInput {
  revision: number;
  providerMode: VoiceProviderMode;
  voiceId: string;
  rate: number;
  pitchHz: number;
  amountBands: AmountBand[];
  chimeEnabled: boolean;
  loadingSoundEnabled: boolean;
  funEnabled: boolean;
  includeDescription: boolean;
  customTemplate: string;
}

export const DEFAULT_AMOUNT_BANDS: AmountBand[] = [
  { id: 'small', fromVnd: '1', toVnd: '50000', styleId: 'classic' },
  { id: 'medium', fromVnd: '50000', toVnd: '200000', styleId: 'classic' },
  { id: 'large', fromVnd: '200000', toVnd: '1000000', styleId: 'classic' },
  { id: 'celebrate', fromVnd: '1000000', toVnd: null, styleId: 'classic' },
];

export const DEFAULT_VOICE_PROFILE: VoiceProfile = {
  revision: 1,
  providerMode: 'VIENEU_ONLY',
  voiceId: VIENEU_VOICE_ID,
  rate: 1.00,
  pitchHz: 0,
  amountBands: DEFAULT_AMOUNT_BANDS,
  chimeEnabled: true,
  loadingSoundEnabled: false,
  funEnabled: true,
  includeDescription: false,
  customTemplate: '',
  catalogVersion: 'v1',
  synthesisKey: '',
  coldRequestTimeoutMs: 8000,
  updatedAt: '2026-10-22T00:00:00Z',
};

export function validateVoiceProfile(value: unknown): VoiceProfile {
  if (!value || typeof value !== 'object') throw new Error('VOICE_PROFILE_INVALID');
  const p = value as Record<string, unknown>;
  if (!Number.isSafeInteger(p.revision) || Number(p.revision) <= 0 || p.providerMode !== 'VIENEU_ONLY' ||
      p.voiceId !== VIENEU_VOICE_ID || p.rate !== 1 || p.pitchHz !== 0 ||
      p.catalogVersion !== 'v1' || typeof p.synthesisKey !== 'string' || !/^[a-f0-9]{64}$/.test(p.synthesisKey) ||
      p.coldRequestTimeoutMs !== 8000 || typeof p.updatedAt !== 'string' || typeof p.customTemplate !== 'string' ||
      ['chimeEnabled', 'loadingSoundEnabled', 'funEnabled', 'includeDescription'].some((key) => typeof p[key] !== 'boolean') ||
      !Array.isArray(p.amountBands) || p.amountBands.length !== 4) throw new Error('VOICE_PROFILE_INVALID');
  const ids = ['small', 'medium', 'large', 'celebrate'];
  let lower = '1';
  for (let index = 0; index < 4; index++) {
    const band: unknown = p.amountBands[index];
    if (!band || typeof band !== 'object' || !('id' in band) || band.id !== ids[index] ||
        !('fromVnd' in band) || band.fromVnd !== lower || !('styleId' in band) ||
        !['classic', 'cute', 'funny', 'custom'].includes(String(band.styleId)) || !('toVnd' in band)) throw new Error('VOICE_PROFILE_INVALID');
    if (index === 3) { if (band.toVnd !== null) throw new Error('VOICE_PROFILE_INVALID'); }
    else {
      if (typeof band.toVnd !== 'string' || !/^[1-9][0-9]*$/.test(band.toVnd) || BigInt(band.toVnd) <= BigInt(lower) ||
          BigInt(band.toVnd) > 9007199254740991n) throw new Error('VOICE_PROFILE_INVALID');
      lower = band.toVnd;
    }
  }
  // Every field above is validated before this boundary cast.
  return value as VoiceProfile;
}

export async function fetchVoiceProfile(isPublic = false): Promise<VoiceProfile> {
  const url = isPublic ? '/api/public/v1/voice/profile' : '/api/v1/voice/profile';
  const res = await fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/json' }, cache: 'no-store' });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return validateVoiceProfile(await res.json());
}

export async function saveVoiceSettingsServer(
  input: VoiceSettingsInput
): Promise<VoiceProfile> {
  try {
    const response = await api<unknown>('/voice/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(input) });
    return validateVoiceProfile(response);
  } catch (error) {
    if (error && typeof error === 'object' && 'status' in error && error.status === 409) throw new Error('VOICE_SETTINGS_CONFLICT');
    throw error;
  }
}
