import React from 'react';
import { Outlet, useNavigate } from 'react-router-dom';
import { ViewerHeader } from './ViewerHeader';
import { isPublicViewerHost } from '../../app/runtime-mode';

export const ViewerLayout: React.FC = () => {
  const navigate = useNavigate();
  const isPublic = isPublicViewerHost();

  return (
    <div className="min-h-screen bg-stone-50 text-stone-900 flex flex-col font-sans antialiased">
      <ViewerHeader />

      <main className="flex-1 max-w-7xl w-full mx-auto px-4 sm:px-6 lg:px-8 py-6 sm:py-8">
        <Outlet />
      </main>

      {/* Discrete footer navigation */}
      <footer className="py-8 border-t border-stone-200 text-center text-xs text-stone-500 space-y-3">
        {!isPublic && (
          <div className="flex flex-wrap items-center justify-center gap-x-4 gap-y-2 text-[11px] text-stone-400">
            <button
              type="button"
              role="button"
              onClick={() => navigate('/admin/overview')}
              className="hover:text-stone-700 transition cursor-pointer"
            >
              Tổng quan
            </button>
            <span>&middot;</span>
            <button
              type="button"
              role="button"
              onClick={() => navigate('/admin/connection')}
              className="hover:text-stone-700 transition cursor-pointer"
            >
              Kết nối ACB
            </button>
            <span>&middot;</span>
            <button
              type="button"
              role="button"
              onClick={() => navigate('/transactions')}
              className="text-emerald-700 font-semibold cursor-pointer"
            >
              Giao dịch
            </button>
            <span>&middot;</span>
            <button
              type="button"
              role="button"
              onClick={() => navigate('/admin/notifications')}
              className="hover:text-stone-700 transition cursor-pointer"
            >
              Webhooks
            </button>
            <span>&middot;</span>
            <button
              type="button"
              role="button"
              onClick={() => navigate('/admin/activity?tab=polling')}
              className="hover:text-stone-700 transition cursor-pointer"
            >
              Polling
            </button>
            <span>&middot;</span>
            <button
              type="button"
              role="button"
              onClick={() => navigate('/admin/activity?tab=deliveries')}
              className="hover:text-stone-700 transition cursor-pointer"
            >
              Phân phối
            </button>
            <span>&middot;</span>
            <button
              type="button"
              role="button"
              onClick={() => navigate('/admin/system')}
              className="hover:text-stone-700 transition cursor-pointer"
            >
              Chẩn đoán
            </button>
            <span>&middot;</span>
            <button
              type="button"
              role="button"
              onClick={() => navigate('/admin/activity?tab=audit')}
              className="hover:text-stone-700 transition cursor-pointer"
            >
              Audit
            </button>
          </div>
        )}

        <div className="text-[11px] text-stone-400">
          Hệ thống theo dõi giao dịch &bull; Phiên bản tự động hoá &bull; ACB Webhook Gateway
        </div>
      </footer>
    </div>
  );
};
