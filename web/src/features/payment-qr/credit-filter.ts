import type { BankTransactionCreditData } from '../../realtime/realtime.types';

export type CreditSource = 'REALTIME' | 'CATCH_UP';

export interface LiveCreditGate {
  sessionOpenedAt: number;
  seenIds: Set<string>;
  sources?: readonly string[];
  freshnessMs?: number;
  now?: number;
}

export const DEFAULT_ALLOWED_SOURCES: readonly string[] = ['REALTIME'];
export const PAYMENT_PAGE_ALLOWED_SOURCES: readonly string[] = ['REALTIME', 'CATCH_UP'];
export const DEFAULT_FRESHNESS_MS = 120_000;
export const CLOCK_SKEW_GRACE_MS = 5_000;

export function parseCreditAmount(raw: unknown): number {
  if (typeof raw === 'number') {
    return Number.isFinite(raw) ? raw : 0;
  }
  if (typeof raw !== 'string') {
    return 0;
  }
  const clean = raw.replace(/[^\d.-]/g, '');
  const parsed = parseFloat(clean);
  return Number.isFinite(parsed) ? parsed : 0;
}

export function parseDetectedAt(raw: unknown): number | null {
  if (typeof raw !== 'string' || !raw.trim()) {
    return null;
  }
  const parsed = Date.parse(raw);
  return Number.isNaN(parsed) ? null : parsed;
}

/**
 * Validates whether an incoming bank.transaction.credit realtime event
 * belongs to the currently active listening session, is fresh, and deduplicates
 * by transaction identifier.
 *
 * Side effect: if accepted, d.transactionId (or transactionNumber fallback)
 * is recorded into gate.seenIds.
 */
export function shouldAcceptLiveCredit(
  d: BankTransactionCreditData | null | undefined,
  gate: LiveCreditGate,
): boolean {
  if (!d) return false;

  const txId = (d.transactionId || d.transactionNumber || '').trim();
  if (!txId) return false;

  if (gate.seenIds.has(txId)) return false;

  const allowedSources = gate.sources ?? DEFAULT_ALLOWED_SOURCES;
  const source = (d.source || 'REALTIME').toUpperCase();
  if (!allowedSources.includes(source)) return false;

  const amount = parseCreditAmount(d.credit);
  if (amount <= 0) return false;

  const detectedAtMs = parseDetectedAt(d.detectedAt);
  if (detectedAtMs === null) return false;

  const now = gate.now ?? Date.now();
  const freshnessMs = gate.freshnessMs ?? DEFAULT_FRESHNESS_MS;

  // Reject historical events that are older than freshness window
  if (now - detectedAtMs > freshnessMs) return false;

  // Reject events clearly older than when the user opened this screen,
  // allowing a small clock-skew margin.
  if (detectedAtMs < gate.sessionOpenedAt - CLOCK_SKEW_GRACE_MS) return false;

  gate.seenIds.add(txId);
  return true;
}
