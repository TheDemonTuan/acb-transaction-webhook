export const ROUTES = {
  root: '/',
  transactions: '/transactions',
  transactionDetail: (id: string) => `/transactions/${id}`,
  pay: (id?: string) => (id ? `/pay/${id}` : '/pay'),
  admin: '/admin',
  adminOverview: '/admin/overview',
  adminConnection: '/admin/connection',
  adminNotifications: '/admin/notifications',
  adminActivity: '/admin/activity',
  adminSystem: '/admin/system',
} as const;
