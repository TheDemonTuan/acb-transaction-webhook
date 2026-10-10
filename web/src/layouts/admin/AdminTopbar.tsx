import React from 'react';
import { useNavigate } from 'react-router-dom';
import { Receipt } from 'lucide-react';
import { VoiceToggle } from '../../features/voice-announcements/components/VoiceToggle';
import { ViewerRealtimeStatus } from '../viewer/ViewerRealtimeStatus';

export const AdminTopbar: React.FC = () => {
  const navigate = useNavigate();

  return (
    <header className="bg-white border-b border-stone-200 h-16 flex items-center justify-between px-4 sm:px-6 lg:px-8">
      <div className="flex min-w-0 flex-1 items-center gap-2 sm:gap-3">
        <div className="w-8 h-8 rounded-xl bg-stone-900 text-white flex items-center justify-center font-bold text-sm shadow-xs shrink-0">
          A
        </div>
        <h1 className="truncate text-xs font-bold text-stone-900 sm:text-sm"><span className="sm:hidden">Quản trị</span><span className="hidden sm:inline">SePay Store + payOS</span></h1>
      </div>

      <div className="flex shrink-0 items-center gap-1 sm:gap-3">
        <div className="hidden sm:block">
          <ViewerRealtimeStatus />
        </div>
        <VoiceToggle className="[&_button]:min-h-11 [&_button]:min-w-11 [&_button_span]:hidden sm:[&_button_span]:inline" />
        <button
          type="button"
          onClick={() => navigate('/transactions')}
          className="inline-flex min-h-11 items-center gap-1.5 rounded-xl bg-emerald-600 px-3 py-1.5 text-xs font-semibold text-white shadow-xs transition hover:bg-emerald-700 cursor-pointer"
        >
          <Receipt className="w-3.5 h-3.5" />
          <span className="hidden sm:inline">Transaction Viewer</span>
          <span className="sm:hidden">Viewer</span>
        </button>
      </div>
    </header>
  );
};
