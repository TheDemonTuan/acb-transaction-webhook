import { describe, expect, it } from 'bun:test';
import { readBounded, validateCatalog, type VoiceCatalog } from '../src/features/voice-announcements/voice-catalog';
import { RecentPhraseRing, selectAnnouncementRecipe } from '../src/features/voice-announcements/voice-copy';
import { DEFAULT_VOICE_PROFILE, validateVoiceProfile } from '../src/features/voice-announcements/voice-profile';

const catalog: VoiceCatalog = {
  version: 'v1', amountCore: 'Đã nhận {amount}.', classic: [],
  fragments: ['small', 'medium', 'large', 'celebrate'].flatMap((bandId) => [
    { id: `${bandId}-o`, text: 'Một niềm vui vừa ghé.', styleId: 'cute', bandId },
    { id: `${bandId}-c`, text: 'Cảm ơn quý khách.', styleId: 'cute', bandId },
  ]),
  recipes: ['small', 'medium', 'large', 'celebrate'].flatMap((bandId) => Array.from({ length: 40 }, (_, i) => ({
    id: `${bandId}-${i}`, styleId: 'cute', bandId, openingId: `${bandId}-o`, closingId: `${bandId}-c`, weight: 1,
  }))),
};
const profile = { ...DEFAULT_VOICE_PROFILE, synthesisKey: 'a'.repeat(64),
  amountBands: DEFAULT_VOICE_PROFILE.amountBands.map((band) => ({ ...band, styleId: 'cute' as const })) };
const choose = (amountVnd: string, recentPhraseIds: readonly string[] = []) => selectAnnouncementRecipe({
  transactionId: 'giao-dịch-có-dấu', amountVnd, profileRevision: profile.revision, catalogVersion: 'v1', profile, catalog, recentPhraseIds,
});

describe('voice catalog contract', () => {
  it('uses exact upper-exclusive BigInt boundaries and never another band', () => {
    for (const [amount, band] of [['1', 'small'], ['49999', 'small'], ['50000', 'medium'], ['199999', 'medium'],
      ['200000', 'large'], ['999999', 'large'], ['1000000', 'celebrate'], ['9007199254740991', 'celebrate']]) {
      expect(choose(amount)?.bandId).toBe(band);
    }
    for (const amount of ['-100', '0', '01', '1.5', '9e3', '9007199254740992']) expect(choose(amount)).toBeUndefined();
  });
  it('selects deterministic UTF8 seed and avoids the bounded recent ring', () => {
    const first = choose('50000')!;
    expect(choose('50000')).toEqual(first);
    expect(choose('50000', [first.id])?.id).not.toBe(first.id);
    const ring = new RecentPhraseRing();
    for (let i = 0; i < 100; i++) ring.started(`phrase-${i}`);
    expect(ring.snapshot()).toHaveLength(32);
    expect(ring.snapshot()[0]).toBe('phrase-68');
    ring.started('phrase-70');
    expect(ring.snapshot().at(-1)).toBe('phrase-70');
    expect(ring.snapshot()).toHaveLength(32);
  });
  it('reserves current and next recipes independently of heard tails and releases failed preparations', () => {
    const ring = new RecentPhraseRing(), current = {}, next = {};
    const first = choose('50000', ring.snapshot())!;
    ring.reserve(current, first.id);
    const second = choose('50000', ring.snapshot())!;
    expect(second.id).not.toBe(first.id);
    ring.reserve(next, second.id);
    expect(() => ring.reserve({}, 'third')).toThrow('VOICE_PREPARATION_CAPACITY');
    ring.release(current);
    expect(ring.snapshot()).not.toContain(first.id);
    ring.started(second.id); ring.release(next);
    expect(ring.snapshot()).toEqual([second.id]);
  });
  it('returns amount-only on failed catalog, missing style pool or disabled fun', () => {
    const input = { transactionId: 'tx', amountVnd: '50000', profileRevision: 1, catalogVersion: 'v1' as const, recentPhraseIds: [], profile, catalog };
    expect(selectAnnouncementRecipe({ ...input, catalog: null })).toBeUndefined();
    expect(selectAnnouncementRecipe({ ...input, profile: { ...profile, funEnabled: false } })).toBeUndefined();
    expect(selectAnnouncementRecipe({ ...input, catalog: { ...catalog, recipes: [] } })).toBeUndefined();
  });
  it('rejects incompatible fragments and profile gaps before selection', () => {
    expect(validateCatalog(catalog, 'v1')).toBe(catalog);
    expect(() => validateCatalog({ ...catalog, fragments: [] }, 'v1')).toThrow('VOICE_CATALOG_INVALID');
    expect(validateVoiceProfile(profile)).toBe(profile);
    expect(() => validateVoiceProfile({ ...profile, amountBands: profile.amountBands.map((band, index) => index === 1 ? { ...band, fromVnd: '50001' } : band) })).toThrow('VOICE_PROFILE_INVALID');
  });
  it('caps chunked JSON and cancels body readers on abort', async () => {
    const controller = new AbortController();
    const cancel = { calls: 0 };
    const response = new Response(new ReadableStream({ start(stream) { stream.enqueue(new Uint8Array(9)); },
      cancel() { cancel.calls++; } }));
    await expect(readBounded(response, 8, controller.signal)).rejects.toThrow('VOICE_RESPONSE_TOO_LARGE');
    expect(cancel.calls).toBe(1);
    const next = new AbortController(); next.abort();
    await expect(readBounded(new Response('text'), 8, next.signal)).rejects.toThrow('VOICE_CANCELLED');
  });
});
