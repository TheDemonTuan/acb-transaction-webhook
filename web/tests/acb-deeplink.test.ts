import { describe, expect, it } from 'vitest';
import { acbDeeplink } from '../src/features/payment-qr/acb-deeplink';

describe('acb-deeplink', () => {
  it('formats VietQR compliant ACB ONE deeplink without 970416 prefix in ba', () => {
    const link = acbDeeplink('12345678', 'NGUYEN VAN A');
    expect(link).toContain('https://dl.vietqr.io/pay?');

    const url = new URL(link);
    expect(url.searchParams.get('app')).toBe('acb');
    expect(url.searchParams.get('ba')).toBe('12345678@acb');
    expect(url.searchParams.get('bn')).toBe('NGUYEN VAN A');
    expect(link).not.toContain('970416');
  });

  it('omits bn parameter when recipient name is not provided', () => {
    const link = acbDeeplink('88889999');
    const url = new URL(link);
    expect(url.searchParams.get('app')).toBe('acb');
    expect(url.searchParams.get('ba')).toBe('88889999@acb');
    expect(url.searchParams.has('bn')).toBe(false);
  });

  it('falls back to acbone:// when account number is missing or blank', () => {
    expect(acbDeeplink('')).toBe('acbone://');
    expect(acbDeeplink(null)).toBe('acbone://');
    expect(acbDeeplink(undefined)).toBe('acbone://');
    expect(acbDeeplink('   ')).toBe('acbone://');
  });

  it('properly encodes special characters and accents in account name', () => {
    const link = acbDeeplink('123456', 'NGUYỄN VĂN AN');
    const url = new URL(link);
    expect(url.searchParams.get('bn')).toBe('NGUYỄN VĂN AN');
  });
});
