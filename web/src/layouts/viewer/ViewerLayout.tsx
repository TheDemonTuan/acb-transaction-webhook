import React from 'react';
import { Outlet, useNavigate } from 'react-router-dom';
import { ViewerHeader } from './ViewerHeader';
import { isPublicViewerHost } from '../../app/runtime-mode';
import { ROUTES } from '../../app/routes';

export const ViewerLayout: React.FC = () => {
  const navigate = useNavigate();
  const isPublic = isPublicViewerHost();

  return (
    <div className="min-h-screen bg-stone-50 text-stone-900 flex flex-col font-sans antialiased">
      <ViewerHeader />

      <main className="flex-1 max-w-[1280px] w-full mx-auto px-3 sm:px-6 py-4 sm:py-6">
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
              Kết nối payOS
            </button>
            <span>&middot;</span>
            <button
              type="button"
              role="button"
              onClick={() => navigate(ROUTES.transactions())}
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
          Theo dõi giao dịch &bull; SePay Store + payOS
        </div>
      </footer>
    </div>
  );
};
