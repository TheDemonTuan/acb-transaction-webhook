import { speakVnd } from './money-to-vietnamese';
import type { VoiceProfile } from './voice-profile';
import type { CatalogRecipe, VoiceCatalog } from './voice-catalog';

export interface AnnouncementRecipe extends CatalogRecipe {
  tail: string;
  fragmentIds: string[];
  template?: string;
}

export function selectAnnouncementRecipe(input: {
  transactionId: string;
  amountVnd: string;
  profileRevision: number;
  catalogVersion: 'v1';
  recentPhraseIds: readonly string[];
  profile: Readonly<VoiceProfile>;
  catalog: VoiceCatalog | null;
}): AnnouncementRecipe | undefined {
  if (!/^[1-9][0-9]*$/.test(input.amountVnd) || !input.profile.funEnabled || !input.catalog) return;
  const amount = BigInt(input.amountVnd);
  if (amount > 9007199254740991n) return;
  const band = input.profile.amountBands.find((b) => amount >= BigInt(b.fromVnd) && (b.toVnd === null || amount < BigInt(b.toVnd)));
  if (!band || band.styleId === 'custom') return;
  const pool = input.catalog.recipes.filter((r) => r.styleId === band.styleId && (r.bandId === band.id || r.bandId === 'all'));
  if (pool.length === 0) return;
  let hash = 2166136261;
  for (const byte of new TextEncoder().encode(`${input.transactionId}:${input.profileRevision}:${input.catalogVersion}`)) {
    hash = Math.imul(hash ^ byte, 16777619) >>> 0;
  }
  const weight = pool.reduce((sum, r) => sum + r.weight, 0);
  if (!Number.isSafeInteger(weight) || weight <= 0) return;
  let ticket = hash % weight;
  let index = 0;
  while (index < pool.length - 1 && ticket >= pool[index].weight) { ticket -= pool[index].weight; index++; }
  const first = index;
  for (let n = 0; n < Math.min(pool.length, 33); n++) {
    index = (first + n) % pool.length;
    if (!input.recentPhraseIds.includes(pool[index].id)) break;
    if (n === Math.min(pool.length, 33) - 1) index = first;
  }
  const recipe = pool[index];
  const template = input.catalog.classic.find((c) => c.id === recipe.id)?.template;
  const fragmentIds = recipe.styleId === 'classic' ? [] : [recipe.openingId, recipe.closingId];
  const tail = fragmentIds.map((id) => input.catalog!.fragments.find((f) => f.id === id)?.text || '').join(' ');
  return { ...recipe, tail, fragmentIds, template };
}

export class RecentPhraseRing {
  private ids: string[] = [];
  private reservations = new Map<object, string>();
  public snapshot(): readonly string[] { return [...this.ids, ...this.reservations.values()]; }
  public reserve(owner: object, id: string): void {
    if (!this.reservations.has(owner) && this.reservations.size >= 2) throw new Error('VOICE_PREPARATION_CAPACITY');
    this.reservations.set(owner, id);
  }
  public release(owner: object): void { this.reservations.delete(owner); }
  public started(id: string): void {
    this.ids = [...this.ids.filter((previous) => previous !== id), id].slice(-32);
  }
}

export const DEFAULT_ANNOUNCEMENT_TEMPLATE = 'Đã nhận {amount}.';

export interface BuildAnnouncementOptions {
  includeDescription?: boolean;
  maxDescriptionLength?: number;
  template?: string;
}

export interface FormatAnnouncementTemplateParams {
  amount: string | number | bigint;
  description?: string;
  includeDescription?: boolean;
  maxDescriptionLength?: number;
}

/**
 * Sanitizes transaction description for speech synthesis.
 * Strips URLs, hex strings, long codes, punctuation spam, and control characters.
 */
export function sanitizeSpeechDescription(desc?: string, maxLength = 100): string {
  if (!desc) return '';
  let cleaned = desc
    .replace(/https?:\/\/\S+/gi, '') // Remove URLs
    .replace(/[^\p{L}\p{N}\s,.-]/gu, ' ') // Only letters, numbers, basic punctuation
    .replace(/\s+/g, ' ')
    .trim();

  if (cleaned.length > maxLength) {
    cleaned = cleaned.slice(0, maxLength).trim();
  }

  return cleaned;
}

/**
 * Formats announcement phrase from a template with token replacement.
 * Supported tokens:
 * - {amount}: Spoken Vietnamese words for the amount (via speakVnd)
 * - {amount_raw}: Unformatted/raw numeric amount string
 * - {description}: Sanitized transaction description
 */
export function formatAnnouncementTemplate(
  template: string | undefined,
  params: FormatAnnouncementTemplateParams
): string {
  const tpl = template && template.trim().length > 0 ? template.trim() : DEFAULT_ANNOUNCEMENT_TEMPLATE;
  const spokenAmount = speakVnd(params.amount);
  const rawAmount = typeof params.amount === 'bigint' ? params.amount.toString() : String(params.amount);
  const sanitized = sanitizeSpeechDescription(params.description, params.maxDescriptionLength);

  let result = tpl;
  result = result.replace(/\{amount\}/g, spokenAmount);
  result = result.replace(/\{amount_raw\}/g, rawAmount);

  if (result.includes('{description}')) {
    const hasValidDesc = params.includeDescription !== false && Boolean(sanitized);
    if (!hasValidDesc) {
      result = result
        .replace(/[,;]?\s*(?:Nội dung|nội dung):\s*\{description\}/g, '')
        .replace(/\{description\}/g, '');
    } else {
      result = result.replace(/\{description\}/g, sanitized);
    }
  } else if (params.includeDescription && sanitized) {
    const trimmed = result.trim();
    if (trimmed.endsWith('.')) {
      result = `${trimmed} Nội dung: ${sanitized}.`;
    } else {
      result = `${trimmed}. Nội dung: ${sanitized}.`;
    }
  }

  return result
    .replace(/\s+/g, ' ')
    .replace(/\s+([.,;:!?])/g, '$1')
    .replace(/\.+/g, '.')
    .trim();
}

/**
 * Builds announcement phrase for a single transaction.
 */
export function buildSingleTransactionPhrase(
  amount: string | number | bigint,
  description?: string,
  options: BuildAnnouncementOptions = {}
): string {
  return formatAnnouncementTemplate(options.template, {
    amount,
    description,
    includeDescription: options.includeDescription,
    maxDescriptionLength: options.maxDescriptionLength,
  });
}

/**
 * Builds announcement phrase for a burst of multiple transactions.
 */
export function buildBurstTransactionPhrase(
  count: number,
  totalAmount: string | number | bigint
): string {
  const spokenTotal = speakVnd(totalAmount);
  return `Bạn vừa nhận được ${count} giao dịch mới, tổng cộng ${spokenTotal}.`;
}
