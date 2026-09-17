import { describe, expect, it } from 'vitest';
import {
  selectQRImageURL,
  isQRReadyToDisplay,
  type RawPaymentQRResponse,
} from '../src/features/payment-qr/qr-payload';

describe('qr-payload', () => {
  it('reads top-level imageURL from public API response', () => {
    const publicResp: RawPaymentQRResponse = {
      configured: true,
      hasImage: true,
      imageURL: '/api/public/v1/payment-qr/image?v=1',
      qr: {
        accountName: 'NGUYEN VAN A',
        accountNumber: '123456',
        bankName: 'ACB',
      },
    };
    expect(selectQRImageURL(publicResp)).toBe('/api/public/v1/payment-qr/image?v=1');
    expect(isQRReadyToDisplay(publicResp)).toBe(true);
  });

  it('reads top-level imageURL from admin API response', () => {
    const adminResp: RawPaymentQRResponse = {
      configured: true,
      hasImage: true,
      imageURL: '/api/v1/payment-qr/image?v=5',
      qr: {
        accountName: 'NGUYEN VAN A',
        accountNumber: '123456',
        bankName: 'ACB',
      },
    };
    expect(selectQRImageURL(adminResp)).toBe('/api/v1/payment-qr/image?v=5');
    expect(isQRReadyToDisplay(adminResp)).toBe(true);
  });

  it('falls back safely to nested qr.imageURL if legacy client encounters it', () => {
    const nestedResp: RawPaymentQRResponse = {
      configured: true,
      hasImage: true,
      qr: {
        imageURL: '/legacy/qr.png',
      },
    };
    expect(selectQRImageURL(nestedResp)).toBe('/legacy/qr.png');
    expect(isQRReadyToDisplay(nestedResp)).toBe(true);
  });

  it('returns undefined when not configured or uninitialized', () => {
    expect(selectQRImageURL(null)).toBeUndefined();
    expect(selectQRImageURL(undefined)).toBeUndefined();
    expect(selectQRImageURL({ configured: false })).toBeUndefined();
    expect(isQRReadyToDisplay({ configured: false })).toBe(false);
  });

  it('isQRReadyToDisplay requires hasImage to be true', () => {
    const noImg: RawPaymentQRResponse = {
      configured: true,
      hasImage: false,
      imageURL: '/api/v1/payment-qr/image?v=1',
    };
    expect(isQRReadyToDisplay(noImg)).toBe(false);
  });
});
