import { speakVnd } from './money-to-vietnamese';

export const DEFAULT_ANNOUNCEMENT_TEMPLATE = 'Đa tạ quý khách vì {amount}.';

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
