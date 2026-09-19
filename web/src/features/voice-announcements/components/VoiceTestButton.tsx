import React, { useState } from 'react';
import { Play } from 'lucide-react';
import { useVoiceAnnouncements } from '../VoiceAnnouncementProvider';

export interface VoiceTestButtonProps {
  className?: string;
  onError?: (err: unknown) => void;
}

export const VoiceTestButton: React.FC<VoiceTestButtonProps> = ({ className = '', onError }) => {
  const { testVoice, unlockAudio, isSupported } = useVoiceAnnouncements();
  const [testing, setTesting] = useState(false);
  const [errorMessage, setErrorMessage] = useState<string | null>(null);

  const handleClick = async () => {
    setTesting(true);
    setErrorMessage(null);
    try {
      const unlocked = await unlockAudio();
      if (!unlocked) {
        throw new Error('AUDIO_UNLOCK_FAILED');
      }
      await testVoice();
    } catch (err: any) {
      const msg =
        err?.message === 'BROWSER_VIETNAMESE_VOICE_UNAVAILABLE'
          ? 'Thiết bị chưa có giọng đọc Tiếng Việt'
          : err?.message === 'AUDIO_UNLOCK_FAILED'
          ? 'Không thể mở khóa âm thanh'
          : err?.message || 'Lỗi phát âm thanh';
      setErrorMessage(msg);
      onError?.(err);
    } finally {
      setTimeout(() => setTesting(false), 800);
    }
  };

  if (!isSupported) return null;

  return (
    <div className="flex flex-col items-start gap-1">
      <button
        type="button"
        onClick={handleClick}
        disabled={testing}
        className={`inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-sm font-medium bg-stone-100 hover:bg-stone-200 text-stone-700 transition cursor-pointer disabled:opacity-50 ${className}`}
      >
        <Play className="w-3.5 h-3.5 fill-current" />
        {testing ? 'Đang phát...' : 'Nghe thử'}
      </button>
      {errorMessage && (
        <span className="text-xs text-rose-600 font-medium" role="alert">
          {errorMessage}
        </span>
      )}
    </div>
  );
};
