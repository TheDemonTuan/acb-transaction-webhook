export const ADMIN_ORIGIN = 'https://bank.tuannguyenviet.site';
export const PUBLIC_VIEWER_HOST = 'transactions.tuannguyenviet.site';

export function isPublicViewerHost(
  hostname = typeof window !== 'undefined' ? window.location.hostname : '',
): boolean {
  return hostname === PUBLIC_VIEWER_HOST;
}
