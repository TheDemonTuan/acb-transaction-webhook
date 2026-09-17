export interface RawPaymentQRResponse {
  configured?: boolean;
  hasImage?: boolean;
  imageURL?: string;
  qr?: {
    accountName?: string;
    accountNumber?: string;
    bankName?: string;
    imageURL?: string; // legacy/defensive shape
  } | null;
}

/**
 * Normalizes payment QR image URL access.
 * Backend returns `imageURL` at the top level of both public (/api/public/v1/payment-qr)
 * and admin (/api/v1/payment-qr) responses. If legacy or nested, falls back safely.
 */
export function selectQRImageURL(
  payload: RawPaymentQRResponse | null | undefined,
): string | undefined {
  if (!payload || !payload.configured) return undefined;
  if (typeof payload.imageURL === 'string' && payload.imageURL.trim()) {
    return payload.imageURL.trim();
  }
  if (payload.qr && typeof payload.qr.imageURL === 'string' && payload.qr.imageURL.trim()) {
    return payload.qr.imageURL.trim();
  }
  return undefined;
}

export function isQRReadyToDisplay(
  payload: RawPaymentQRResponse | null | undefined,
): boolean {
  if (!payload || !payload.configured) return false;
  return Boolean(payload.hasImage && selectQRImageURL(payload));
}
