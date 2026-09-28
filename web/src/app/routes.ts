import { isPublicViewerHost } from './runtime-mode';

export const ROUTES = {
  root: '/',
  transactions: (isPublic = isPublicViewerHost()) => isPublic ? '/' : '/transactions',
  transactionDetail: (id: string, isPublic = isPublicViewerHost()) =>
    `${isPublic ? '/t' : '/transactions'}/${encodeURIComponent(id)}`,
  admin: '/admin',
  adminOverview: '/admin/overview',
  adminConnection: '/admin/connection',
  adminNotifications: '/admin/notifications',
  adminActivity: '/admin/activity',
  adminSystem: '/admin/system',
} as const;
