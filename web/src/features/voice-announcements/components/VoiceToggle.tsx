import React, { useState } from 'react';
import { Volume2, VolumeX, Settings2 } from 'lucide-react';
import { useVoiceAnnouncements } from '../VoiceAnnouncementProvider';
import { VoiceSettingsSheet } from './VoiceSettingsSheet';

export const VoiceToggle: React.FC<{ className?: string }> = ({ className = '' }) => {
  const { settings, updateSettings, unlockAudio, isSupported } = useVoiceAnnouncements();
  const [sheetOpen, setSheetOpen] = useState(false);

  return (
    <>
      <div className={`inline-flex items-center shrink-0 flex-nowrap gap-1 bg-stone-100 p-1 rounded-xl border border-stone-200/80 ${className}`}>
        <button
          type="button"
          onClick={async () => {
            if (!settings.enabled) {
              try {
                const unlocked = await unlockAudio();
                if (!unlocked) {
                  setSheetOpen(true);
                  return;
                }
                updateSettings({ enabled: true });
              } catch {
                setSheetOpen(true);
              }
            } else {
              updateSettings({ enabled: false });
            }
          }}
          className={`flex items-center shrink-0 gap-1.5 px-2.5 sm:px-3 py-1.5 rounded-lg text-xs font-medium transition cursor-pointer whitespace-nowrap ${
            settings.enabled
              ? 'bg-emerald-600 text-white shadow-xs'
              : 'text-stone-700 hover:bg-stone-200/60'
          }`}
          title={isSupported ? 'Bật/Tắt đọc giao dịch' : 'Trình duyệt chưa hỗ trợ phát âm thanh'}
        >
          {settings.enabled ? (
            <>
              <Volume2 className="w-4 h-4 animate-pulse shrink-0" />
              <span>Giọng đọc: Bật</span>
            </>
          ) : (
            <>
              <VolumeX className="w-4 h-4 text-stone-500 shrink-0" />
              <span>Giọng đọc: Tắt</span>
            </>
          )}
        </button>
        <button
          type="button"
          onClick={() => setSheetOpen(true)}
          className="p-1.5 rounded-lg text-stone-500 hover:text-stone-800 hover:bg-stone-200/70 transition cursor-pointer shrink-0"
          title="Tùy chỉnh giọng đọc"
          aria-label="Tùy chỉnh giọng đọc"
        >
          <Settings2 className="w-4 h-4 shrink-0" />
        </button>
      </div>

      <VoiceSettingsSheet isOpen={sheetOpen} onClose={() => setSheetOpen(false)} />
    </>
  );
};
