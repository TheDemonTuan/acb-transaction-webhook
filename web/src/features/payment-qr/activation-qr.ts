import { PUBLIC_VIEWER_HOST } from '../../app/runtime-mode';
import { ROUTES } from '../../app/routes';

export const ACTIVATION_IDENTIFIER_TTL_MS = 10 * 60 * 1000; // 10 minutes

/**
 * Generates a standard random UUID identifier suitable for activation.
 * Follows [a-zA-Z0-9_-] server validation contract.
 */
export function newActivationIdentifier(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  // Fallback pseudorandom token
  const bytes = new Uint8Array(16);
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    crypto.getRandomValues(bytes);
  } else {
    for (let i = 0; i < 16; i++) bytes[i] = Math.floor(Math.random() * 256);
  }
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

/**
 * Validates identifier string against server safety rules:
 * non-empty, 8..128 characters, characters in [a-zA-Z0-9_-].
 */
export function isSafeActivationIdentifier(s: unknown): boolean {
  if (typeof s !== 'string') return false;
  const trimmed = s.trim();
  if (trimmed.length < 8 || trimmed.length > 128) return false;
  for (let i = 0; i < trimmed.length; i++) {
    const c = trimmed.charCodeAt(i);
    const isLower = c >= 97 && c <= 122;
    const isUpper = c >= 65 && c <= 90;
    const isDigit = c >= 48 && c <= 57;
    const isDash = c === 45;
    const isUnderscore = c === 95;
    if (!isLower && !isUpper && !isDigit && !isDash && !isUnderscore) {
      return false;
    }
  }
  return true;
}

/**
 * Constructs the canonical public activation landing URL.
 * Always resolves to the public transaction viewer domain.
 */
export function activationURL(identifier: string, host = PUBLIC_VIEWER_HOST): string {
  const cleanId = encodeURIComponent(identifier.trim());
  return `https://${host}${ROUTES.pay(cleanId)}`;
}
