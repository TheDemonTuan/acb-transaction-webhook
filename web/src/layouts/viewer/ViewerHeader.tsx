import React from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { Shield, ArrowRight, QrCode } from 'lucide-react';
import { ViewerRealtimeStatus } from './ViewerRealtimeStatus';
import { VoiceToggle } from '../../features/voice-announcements/components/VoiceToggle';
import { ADMIN_ORIGIN, isPublicViewerHost } from '../../app/runtime-mode';
import { ROUTES } from '../../app/routes';

export const ViewerHeader: React.FC = () => {
  const navigate = useNavigate();

  const handleAdminClick = () => {
    if (isPublicViewerHost()) {
      window.location.assign(`${ADMIN_ORIGIN}/admin`);
    } else {
      navigate('/admin');
    }
  };

  return (
    <header className="bg-white border-b border-stone-200">
      <div className="max-w-7xl mx-auto px-3 sm:px-6 h-[63px] flex items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-3">
          <Link to={ROUTES.transactions()} className="flex shrink-0 items-center gap-2 group">
            <div className="w-9 h-9 shrink-0 rounded-xl bg-emerald-600 text-white flex items-center justify-center font-bold text-lg shadow-sm group-hover:bg-emerald-700 transition">
              A
            </div>
            <div>
              <h1 className="text-sm sm:text-base font-bold text-stone-900 leading-tight whitespace-nowrap">
                Thu ngân
              </h1>
              <span className="text-[11px] font-medium text-stone-500 hidden sm:block">
                SePay Store + payOS
              </span>
            </div>
          </Link>
        </div>

        <div className="flex items-center gap-2">
          <div className="hidden sm:block">
            <ViewerRealtimeStatus />
          </div>

          <a href={`${ROUTES.transactions()}#counter-checkout`} className="min-h-11 min-w-11 inline-flex items-center justify-center gap-1.5 px-2 sm:px-3 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 focus-visible:outline-2 focus-visible:outline-emerald-600" aria-label="Thu tiền">
            <QrCode className="w-4 h-4 text-emerald-700" />
            <span className="hidden sm:inline">Thu tiền</span>
          </a>

          <VoiceToggle className="[&_button]:min-h-11 [&_button]:min-w-11 [&_button_span]:hidden sm:[&_button_span]:inline" />

          {!isPublicViewerHost() && (
            <button
              type="button"
              role="button"
              aria-label="Quản trị"
              onClick={handleAdminClick}
              className="min-h-11 min-w-11 inline-flex items-center justify-center gap-1.5 px-2 sm:px-3.5 py-2 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 transition shadow-xs cursor-pointer"
            >
              <Shield className="w-3.5 h-3.5 text-emerald-400" />
              <span className="hidden sm:inline">Quản trị</span>
              <ArrowRight className="hidden sm:block w-3 h-3 text-stone-400" />
            </button>
          )}
        </div>
      </div>

    </header>
  );
};
