import React from 'react';
import { Volume2, VolumeX } from 'lucide-react';
import { useVoiceAnnouncements } from '../VoiceAnnouncementProvider';

export const VoiceStatus: React.FC<{ className?: string }> = ({ className = '' }) => {
  const { isSupported, settings, audioUnlocked, isSpeaking, voiceState, voiceDetail, profileOffline, pendingCount, skippedCount } = useVoiceAnnouncements();

  if (!isSupported) {
    return (
      <span className={`inline-flex items-center gap-1.5 text-xs text-stone-400 ${className}`}>
        <VolumeX className="w-3.5 h-3.5" />
        Thiết bị chưa hỗ trợ đọc
      </span>
    );
  }

  if (!settings.enabled) {
    return (
      <span className={`inline-flex items-center gap-1.5 text-xs text-stone-500 ${className}`}>
        <VolumeX className="w-3.5 h-3.5" />
        Giọng đọc: Tắt
      </span>
    );
  }

  return (
    <span
      title={voiceDetail}
      className={`inline-flex items-center gap-1.5 text-xs font-medium text-emerald-700 bg-emerald-50 px-2 py-0.5 rounded-full border border-emerald-200 ${className}`}
    >
      <Volume2 className={`w-3.5 h-3.5 text-emerald-600 ${isSpeaking ? 'animate-bounce' : ''}`} />
      Giọng đọc: {!audioUnlocked ? 'Cần mở khóa loa' : voiceState === 'ready' ? 'Sẵn sàng' : voiceState === 'cold' ? 'Tổng hợp trực tuyến' : 'Không khả dụng'}
      {profileOffline ? ' (hồ sơ offline)' : ''} · Chờ {pendingCount} · Bỏ qua {skippedCount}
    </span>
  );
};
