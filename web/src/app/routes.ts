export const ROUTES = {
  root: '/',
  transactions: '/transactions',
  transactionDetail: (id: string) => `/transactions/${id}`,
  admin: '/admin',
  adminOverview: '/admin/overview',
  adminConnection: '/admin/connection',
  adminNotifications: '/admin/notifications',
  adminActivity: '/admin/activity',
  adminSystem: '/admin/system',
} as const;
