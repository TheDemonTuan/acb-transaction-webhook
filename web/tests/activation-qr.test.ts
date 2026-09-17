import { describe, expect, it } from 'vitest';
import {
  newActivationIdentifier,
  isSafeActivationIdentifier,
  activationURL,
} from '../src/features/payment-qr/activation-qr';
import { PUBLIC_VIEWER_HOST } from '../src/app/runtime-mode';

describe('activation-qr', () => {
  it('generates safe UUID-like identifier', () => {
    const id = newActivationIdentifier();
    expect(id).toBeTypeOf('string');
    expect(id.length).toBeGreaterThanOrEqual(16);
    expect(isSafeActivationIdentifier(id)).toBe(true);
  });

  it('validates safe activation identifiers strictly', () => {
    expect(isSafeActivationIdentifier('order-12345')).toBe(true);
    expect(isSafeActivationIdentifier('c56a4180-65aa-42ec-a945-5fd21dec0538')).toBe(true);
    expect(isSafeActivationIdentifier('PAY_TX_2026_09')).toBe(true);

    // Too short (< 8)
    expect(isSafeActivationIdentifier('short')).toBe(false);
    expect(isSafeActivationIdentifier('')).toBe(false);

    // Forbidden characters
    expect(isSafeActivationIdentifier('has spaces in id')).toBe(false);
    expect(isSafeActivationIdentifier('bad/char/slug')).toBe(false);
    expect(isSafeActivationIdentifier('inject?evil=1')).toBe(false);
    expect(isSafeActivationIdentifier('semi;colon')).toBe(false);
    expect(isSafeActivationIdentifier(null)).toBe(false);
    expect(isSafeActivationIdentifier(undefined)).toBe(false);
  });

  it('builds canonical activation URL targeting public viewer host', () => {
    const id = 'test-token-uuid-1234';
    const url = activationURL(id);
    expect(url).toBe(`https://${PUBLIC_VIEWER_HOST}/pay/test-token-uuid-1234`);
  });

  it('encodes special characters in identifier', () => {
    const id = 'order_123-abc';
    const url = activationURL(id);
    expect(url).toBe(`https://${PUBLIC_VIEWER_HOST}/pay/order_123-abc`);
  });
});
