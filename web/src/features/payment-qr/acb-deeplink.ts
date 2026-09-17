/**
 * Constructs a VietQR universal mobile deeplink for ACB ONE.
 *
 * Per VietQR specifications (https://www.vietqr.io/danh-sach-api/deeplink-app-ngan-hang/):
 *   - app: target banking app identifier ('acb')
 *   - ba:  account identifier formatted as `SO_TAI_KHOAN@MA_NGAN_HANG` (e.g. `123456@acb`)
 *   - bn:  recipient account name (optional)
 *
 * Fallback: if no account number is available, returns the direct ACB ONE app scheme `acbone://`.
 */
export function acbDeeplink(
  accountNumber?: string | null,
  accountName?: string | null,
): string {
  const cleanAcc = (accountNumber || '').trim();
  if (!cleanAcc) {
    return 'acbone://';
  }

  const params = new URLSearchParams({
    app: 'acb',
    ba: `${cleanAcc}@acb`,
  });

  const cleanName = (accountName || '').trim();
  if (cleanName) {
    params.set('bn', cleanName);
  }

  return `https://dl.vietqr.io/pay?${params.toString()}`;
}
