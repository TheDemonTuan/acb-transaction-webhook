import React from 'react';
import { createBrowserRouter, Navigate } from 'react-router-dom';
import { AdminLayout } from '../layouts/admin/AdminLayout';
import { ViewerLayout } from '../layouts/viewer/ViewerLayout';
import { OverviewPage } from '../pages/admin/OverviewPage';
import { BankConnectionPage } from '../pages/admin/BankConnectionPage';
import { NotificationChannelsPage } from '../pages/admin/NotificationChannelsPage';
import { ActivityPage } from '../pages/admin/ActivityPage';
import { SystemPage } from '../pages/admin/SystemPage';
import { TransactionsPage } from '../pages/viewer/TransactionsPage';
import { TransactionDetailPage } from '../pages/viewer/TransactionDetailPage';
import { isPublicViewerHost } from './runtime-mode';

export const publicRoutes = [
  {
    path: '/',
    element: <Navigate to="/transactions" replace />,
  },
  {
    path: '/transactions',
    element: <ViewerLayout />,
    children: [
      { index: true, element: <TransactionsPage /> },
      { path: ':id', element: <TransactionDetailPage /> },
    ],
  },
  {
    path: '*',
    element: <Navigate to="/transactions" replace />,
  },
];

export const adminRoutes = [
  {
    path: '/',
    element: <AdminLayout />,
    children: [
      { index: true, element: <OverviewPage /> },
      { path: 'admin', element: <OverviewPage /> },
      { path: 'admin/overview', element: <OverviewPage /> },
      { path: 'admin/connection', element: <BankConnectionPage /> },
      { path: 'admin/notifications', element: <NotificationChannelsPage /> },
      { path: 'admin/activity', element: <ActivityPage /> },
      { path: 'admin/system', element: <SystemPage /> },
    ],
  },
  {
    path: '/transactions',
    element: <ViewerLayout />,
    children: [
      { index: true, element: <TransactionsPage /> },
      { path: ':id', element: <TransactionDetailPage /> },
    ],
  },
  {
    path: '*',
    element: <Navigate to="/" replace />,
  },
];

export function createRouter(isPublic = isPublicViewerHost()) {
  return createBrowserRouter(isPublic ? publicRoutes : adminRoutes);
}

export const router = typeof document !== 'undefined'
  ? createRouter()
  : (null as any);
