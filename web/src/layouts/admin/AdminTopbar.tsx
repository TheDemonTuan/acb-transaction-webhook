import React from 'react';
import { useNavigate } from 'react-router-dom';
import { Receipt } from 'lucide-react';
import { VoiceToggle } from '../../features/voice-announcements/components/VoiceToggle';
import { ViewerRealtimeStatus } from '../viewer/ViewerRealtimeStatus';

export const AdminTopbar: React.FC = () => {
  const navigate = useNavigate();

  return (
    <header className="bg-white border-b border-stone-200 h-16 flex items-center justify-between px-4 sm:px-6 lg:px-8">
      <div className="flex items-center gap-2 sm:gap-3 shrink-0">
        <div className="w-8 h-8 rounded-xl bg-stone-900 text-white flex items-center justify-center font-bold text-sm shadow-xs shrink-0">
          P
        </div>
        <h1 className="text-xs sm:text-sm font-bold text-stone-900 truncate">payOS Transaction Webhook</h1>
      </div>

      <div className="flex items-center gap-3">
        <div className="hidden sm:block">
          <ViewerRealtimeStatus />
        </div>
        <VoiceToggle />
        <button
          type="button"
          onClick={() => navigate('/transactions')}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-semibold bg-emerald-600 text-white hover:bg-emerald-700 transition shadow-xs cursor-pointer"
        >
          <Receipt className="w-3.5 h-3.5" />
          <span className="hidden sm:inline">Transaction Viewer</span>
          <span className="sm:hidden">Viewer</span>
        </button>
      </div>
    </header>
  );
};
