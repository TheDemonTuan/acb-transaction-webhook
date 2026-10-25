import React, { useState, useEffect } from 'react';
import { Volume2, VolumeX, X, Sliders, CheckCircle2, AlertCircle } from 'lucide-react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useVoiceAnnouncements } from '../VoiceAnnouncementProvider';
import { isVoiceCancellation } from '../voice-engine';
import { VoiceTestButton } from './VoiceTestButton';
import { exportVoiceTelemetry } from '../voice-telemetry';
import {
  saveVoiceSettingsServer,
  DEFAULT_AMOUNT_BANDS,
  VIENEU_VOICE_LABEL,
  type VoiceProfile,
  type VoiceSettingsInput,
} from '../voice-profile';
import { queryKeys } from '../../../shared/api/query-keys';
import { fetchStatus } from '../../../shared/api/queries';
export interface VoiceSettingsSheetProps {
  isOpen: boolean;
  onClose: () => void;
}

export const VoiceSettingsSheet: React.FC<VoiceSettingsSheetProps> = ({ isOpen, onClose }) => {
  const queryClient = useQueryClient();
  const { settings, updateSettings, unlockAudio, audioUnlocked, isSupported, lastTelemetry, profile: activeProfile, voiceState, voiceDetail, profileOffline, recentSkipped, replayVoice } = useVoiceAnnouncements();
  const [toggleError, setToggleError] = useState<string | null>(null);
  const [serverSaveError, setServerSaveError] = useState<string | null>(null);
  const [serverSaveSuccess, setServerSaveSuccess] = useState(false);
  const [isSavingServer, setIsSavingServer] = useState(false);


  const handleDownloadTelemetry = () => {
    const data = exportVoiceTelemetry();
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `voice-telemetry-${new Date().toISOString().slice(0, 19).replace(/:/g, '-')}.json`;
    a.click();
    URL.revokeObjectURL(url);
  };
  const statusQuery = useQuery({ queryKey: queryKeys.status, queryFn: fetchStatus });
  const isOwnerOrOperator =
    statusQuery.data?.userRole === 'OWNER' || statusQuery.data?.userRole === 'OPERATOR';

  const profileQuery = { data: activeProfile };

  const [formDraft, setFormDraft] = useState<VoiceSettingsInput | null>(null);

  const [legacyImported, setLegacyImported] = useState(false);
  useEffect(() => {
    if (profileQuery.data && !formDraft) {
      setFormDraft({
        revision: profileQuery.data.revision,
        providerMode: profileQuery.data.providerMode,
        voiceId: profileQuery.data.voiceId,
        rate: profileQuery.data.rate,
        pitchHz: profileQuery.data.pitchHz,
        amountBands: profileQuery.data.amountBands || DEFAULT_AMOUNT_BANDS,
        chimeEnabled: profileQuery.data.chimeEnabled,
        loadingSoundEnabled: profileQuery.data.loadingSoundEnabled,
        funEnabled: profileQuery.data.funEnabled,
        includeDescription: profileQuery.data.includeDescription,
        customTemplate: profileQuery.data.customTemplate,
      });
    }
  }, [profileQuery.data, formDraft]);

  if (!isOpen) return null;

  const handleToggle = async () => {
    setToggleError(null);
    if (!settings.enabled || !audioUnlocked) {
      try {
        const unlocked = await unlockAudio();
        if (!unlocked) {
          setToggleError('Trình duyệt chặn phát âm thanh tự động. Hãy bấm Nghe thử hoặc tương tác để mở khóa.');
          return;
        }
        updateSettings({ enabled: true });
      } catch (err: unknown) {
        const msg = err && typeof err === 'object' && 'message' in err ? String(err.message) : 'Không thể kích hoạt âm thanh';
        setToggleError(msg);
      }
    } else {
      updateSettings({ enabled: false });
    }
  };

  const handleImportLegacy = () => {
    if (!settings.legacySnapshot || !formDraft) return;
    const snap = settings.legacySnapshot;
    setLegacyImported(true);
    setFormDraft((prev) => {
      if (!prev) return prev;
      return {
        ...prev,
        customTemplate: snap.announcementTemplate || prev.customTemplate,
        includeDescription: Boolean(snap.includeDescription ?? prev.includeDescription),
      };
    });
  };

  const handleSaveServer = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formDraft) return;
    setServerSaveError(null);
    setServerSaveSuccess(false);
    setIsSavingServer(true);
    try {
      const updated = await saveVoiceSettingsServer(formDraft);
      setFormDraft({
        revision: updated.revision,
        providerMode: updated.providerMode,
        voiceId: updated.voiceId,
        rate: updated.rate,
        pitchHz: updated.pitchHz,
        amountBands: updated.amountBands,
        chimeEnabled: updated.chimeEnabled,
        loadingSoundEnabled: updated.loadingSoundEnabled,
        funEnabled: updated.funEnabled,
        includeDescription: updated.includeDescription,
        customTemplate: updated.customTemplate,
      });
      // Clear legacy snapshot locally once imported and saved
      if (legacyImported && settings.legacySnapshot) {
        updateSettings({ legacySnapshot: undefined });
      }
      setLegacyImported(false);
      queryClient.setQueryData(['voice-profile'], updated);
      setServerSaveSuccess(true);
    } catch (err: unknown) {
      if (err instanceof Error && err.message === 'VOICE_SETTINGS_CONFLICT') {
        setServerSaveError('Cấu hình máy chủ đã bị thay đổi bởi người khác. Vui lòng tải lại trang.');
      } else {
        const msg = err instanceof Error ? err.message : 'Không thể lưu cấu hình lên máy chủ';
        setServerSaveError(msg);
      }
    } finally {
      setIsSavingServer(false);
    }
  };

  const profile: VoiceProfile | undefined = profileQuery.data;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4">
      <div className="bg-white rounded-2xl shadow-xl border border-stone-200 w-full max-w-lg max-h-[90vh] overflow-y-auto">
        {/* Modal Header */}
        <div className="flex items-center justify-between px-6 py-4 border-b border-stone-100">
          <div className="flex items-center gap-2">
            <div className="p-2 rounded-lg bg-emerald-50 text-emerald-600">
              <Volume2 className="w-5 h-5" />
            </div>
            <div>
              <h3 className="font-semibold text-stone-900">Cài đặt đọc giao dịch</h3>
              <p className="text-xs text-stone-500">
                {profile ? `Hồ sơ máy chủ (rev ${profile.revision})` : 'Âm thanh thông báo'}
              </p>
            </div>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="p-1 rounded-lg text-stone-400 hover:text-stone-600 hover:bg-stone-100 transition cursor-pointer"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        {/* Modal Content */}
        <div className="p-6 space-y-5 text-xs text-stone-700">
          {!isSupported ? (
            <div className="p-4 rounded-xl bg-amber-50 border border-amber-200 text-sm text-amber-800 flex items-start gap-2.5">
              <VolumeX className="w-5 h-5 shrink-0 text-amber-600" />
              <span>Trình duyệt hoặc thiết bị này chưa hỗ trợ phát âm thanh Web Audio.</span>
            </div>
          ) : (
            <>
              {/* Enable Toggle (Local Device) */}
              <div className="p-3 rounded-xl bg-stone-50 border border-stone-200 space-y-2">
                <p>Giọng hiệu lực: {VIENEU_VOICE_LABEL} · 1x · 0Hz</p>
                <p>{voiceState}{profileOffline ? ' · Hồ sơ máy chủ offline, dùng bản đã xác nhận' : ''} {voiceDetail}</p>
                {settings.enabled && !audioUnlocked && <button type="button" onClick={handleToggle} className="rounded border border-amber-300 bg-amber-50 p-2">Mở khóa loa — cần thao tác trên thiết bị này</button>}
                {recentSkipped.length > 0 && <details><summary>Giao dịch chưa đọc ({recentSkipped.length} gần nhất)</summary>
                  {recentSkipped.map((item, index) => <div key={`${item.transactionId}:${index}`} className="flex gap-2">
                    <span>{item.reason}</span><button type="button" onClick={() => {
                      setToggleError(null);
                      void replayVoice(item.transactionId).catch((error: unknown) => {
                        if (isVoiceCancellation(error)) return;
                        setToggleError(error instanceof Error ? error.message : 'Không thể phát lại');
                      });
                    }}>Đọc lại {item.transactionId}</button>
                  </div>)}
                </details>}
              </div>
              <div className="p-3 rounded-xl bg-stone-50 border border-stone-200/70 space-y-2">
                <div className="flex items-center justify-between">
                  <div>
                    <span className="text-sm font-medium text-stone-900 block">
                      Bật loa thiết bị này
                    </span>
                    <span className="text-xs text-stone-500">
                      Tự động phát chuông và đọc số tiền khi có giao dịch
                    </span>
                  </div>
                  <button
                    type="button"
                    role="switch"
                    aria-checked={settings.enabled}
                    aria-label="Bật đọc giao dịch"
                    onClick={handleToggle}
                    className={`relative inline-flex h-6 w-11 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus:ring-2 focus:ring-emerald-500 ${
                      settings.enabled ? 'bg-emerald-600' : 'bg-stone-300'
                    }`}
                  >
                    <span
                      className={`pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow-xs ring-0 transition duration-200 ease-in-out ${
                        settings.enabled ? 'translate-x-5' : 'translate-x-0'
                      }`}
                    />
                  </button>
                </div>
                {toggleError && (
                  <div className="p-2.5 rounded-lg bg-rose-50 border border-rose-200 text-xs text-rose-700 font-medium" role="alert">
                    {toggleError}
                  </div>
                )}
              </div>

              {/* Volume Slider (Local Device) */}
              <div>
                <div className="flex justify-between text-xs font-medium text-stone-700 mb-1">
                  <span>Âm lượng thiết bị</span>
                  <span>{Math.round(settings.volume * 100)}%</span>
                </div>
                <input
                  type="range"
                  min="0"
                  max="1"
                  step="0.05"
                  value={settings.volume}
                  disabled={!settings.enabled}
                  onChange={(e) => updateSettings({ volume: parseFloat(e.target.value) })}
                  className="w-full accent-emerald-600 cursor-pointer disabled:opacity-50"
                />
              </div>

              {/* Announce when hidden */}
              <label className="flex items-start gap-2.5 cursor-pointer select-none">
                <input
                  type="checkbox"
                  checked={settings.announceWhenHidden}
                  disabled={!settings.enabled}
                  onChange={(e) => updateSettings({ announceWhenHidden: e.target.checked })}
                  className="mt-0.5 rounded border-stone-300 text-emerald-600 focus:ring-emerald-500 disabled:opacity-50"
                />
                <div>
                  <span className="text-sm font-medium text-stone-800 block">
                    Đọc cả khi chuyển tab khác
                  </span>
                  <span className="text-xs text-stone-500">
                    Tiếp tục thông báo khi ứng dụng đang mở ở tab nền
                  </span>
                </div>
              </label>

              {/* Server Profile Configuration (Owner / Operator) */}
              {isOwnerOrOperator && formDraft && (
                <form onSubmit={handleSaveServer} className="pt-4 border-t border-stone-100 space-y-4">
                  <div className="flex items-center justify-between">
                    <span className="font-semibold text-stone-900 flex items-center gap-1.5 text-sm">
                      <Sliders className="w-4 h-4 text-emerald-600" />
                      Cấu hình phát âm toàn hệ thống
                    </span>
                    {settings.legacySnapshot && (
                      <button
                        type="button"
                        onClick={handleImportLegacy}
                        className="text-xs text-blue-600 hover:text-blue-800 font-medium underline cursor-pointer"
                      >
                        Nhập cấu hình cũ lên máy chủ
                      </button>
                    )}
                  </div>

                  <div className="rounded-lg border border-stone-200 bg-stone-50 p-3 space-y-1">
                    <p className="font-medium">{VIENEU_VOICE_LABEL}</p>
                    <p>Tốc độ tự nhiên 1x · Cao độ 0 Hz · Không chuyển sang giọng khác</p>
                  </div>

                  {/* Audio Features */}
                  <div className="space-y-2 pt-1">
                    <label className="flex items-center gap-2 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={formDraft.chimeEnabled}
                        onChange={(e) =>
                          setFormDraft((p) => (p ? { ...p, chimeEnabled: e.target.checked } : null))
                        }
                        className="rounded border-stone-300 text-emerald-600"
                      />
                      <span>Phát chuông báo âm thanh trước khi đọc</span>
                    </label>
                    <p className="pl-6 text-xs text-stone-600">
                      Calm by Aarni Koskela (akx) ·{' '}
                      <a
                        href="https://creativecommons.org/licenses/by/3.0/"
                        className="underline hover:text-stone-900 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2"
                      >
                        CC BY 3.0
                      </a>
                      {' · '}
                      <a
                        href="https://github.com/akx/Notifications/blob/db184d33834a94028b6600d155d312ce7b704e8d/WAV/Calm.wav"
                        className="underline hover:text-stone-900 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2"
                      >
                        Nguồn bản ghi Calm (phiên bản cố định)
                      </a>
                      . Bản ghi gốc không chỉnh sửa; chỉ điều chỉnh âm lượng khi phát.
                    </p>

                    <label className="flex items-center gap-2 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={formDraft.funEnabled}
                        onChange={(e) =>
                          setFormDraft((p) => (p ? { ...p, funEnabled: e.target.checked } : null))
                        }
                        className="rounded border-stone-300 text-emerald-600"
                      />
                      <span>Đọc câu cảm ơn / câu vui sau số tiền</span>
                    </label>

                    <label className="flex items-center gap-2 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={formDraft.includeDescription}
                        onChange={(e) =>
                          setFormDraft((p) => (p ? { ...p, includeDescription: e.target.checked } : null))
                        }
                        className="rounded border-stone-300 text-emerald-600"
                      />
                      <span>Đọc kèm nội dung chuyển khoản</span>
                    </label>
                  </div>

                  {/* Amount Band Styles */}
                  <div className="pt-1 space-y-1.5">
                    <span className="block font-medium text-stone-800">Phong cách câu theo khung tiền</span>
                    <label>Áp dụng phong cách cho mọi khung
                      <select defaultValue="" onChange={(event) => {
                        const style = event.target.value;
                        if (style === 'classic' || style === 'cute' || style === 'funny' || style === 'custom') {
                          setFormDraft((draft) => draft ? { ...draft, amountBands: draft.amountBands.map((band) => ({ ...band, styleId: style })) } : null);
                        }
                      }} className="ml-2 rounded border p-1">
                        <option value="">Chọn phong cách</option><option value="classic">Cổ điển</option><option value="cute">Dễ thương</option><option value="funny">Hài hước</option><option value="custom">Câu riêng</option>
                      </select>
                    </label>
                    <div className="grid grid-cols-2 gap-2 text-[11px]">
                      {formDraft.amountBands.map((band, idx) => (
                        <div key={band.id} className="p-2 rounded-lg bg-stone-50 border border-stone-200">
                          <span className="font-semibold block capitalize">{band.id}:</span>
                          {band.toVnd !== null && <label>Đến (không gồm)
                            <input value={band.toVnd} inputMode="numeric" className="w-full rounded border p-1" onChange={(event) => {
                              const threshold = event.target.value;
                              setFormDraft((draft) => {
                                if (!draft) return null;
                                const bands = draft.amountBands.map((entry) => ({ ...entry }));
                                bands[idx].toVnd = threshold; bands[idx + 1].fromVnd = threshold;
                                return { ...draft, amountBands: bands };
                              });
                            }} />
                          </label>}
                          <select
                            value={band.styleId}
                            onChange={(e) => {
                              const nextBands = [...formDraft.amountBands];
                              nextBands[idx] = {
                                ...band,
                                styleId: e.target.value as 'classic' | 'cute' | 'funny' | 'custom',
                              };
                              setFormDraft((p) => (p ? { ...p, amountBands: nextBands } : null));
                            }}
                            className="mt-1 w-full p-1 bg-white border border-stone-200 rounded"
                          >
                            <option value="classic">Cổ điển (Classic)</option>
                            <option value="cute">Dễ thương (Cute)</option>
                            <option value="funny">Hài hước (Funny)</option>
                            <option value="custom">Riêng (Custom)</option>
                          </select>
                        </div>
                      ))}
                    </div>
                  </div>

                  {/* Custom Template */}
                  <div>
                    <label className="block font-medium text-stone-800 mb-1">
                      Câu riêng — có thể cần tổng hợp tại máy chủ
                    </label>
                    <input
                      type="text"
                      value={formDraft.customTemplate}
                      placeholder="Ví dụ: Đã nhận {amount} từ khách quý."
                      onChange={(e) =>
                        setFormDraft((p) => (p ? { ...p, customTemplate: e.target.value } : null))
                      }
                      className="w-full text-xs rounded-lg border border-stone-300 p-2 bg-white text-stone-800"
                    />
                    <span className="text-[10px] text-stone-400 block mt-0.5">
                      Đúng một {'{amount}'} hoặc {'{amount_raw}'}. Có thể kèm {'{description}'}. Tối đa 120 ký tự Unicode.
                    </span>
                  </div>

                  {serverSaveError && (
                    <div className="p-2.5 rounded-lg bg-rose-50 border border-rose-200 text-xs text-rose-700 flex items-center gap-1.5">
                      <AlertCircle className="w-4 h-4 shrink-0" />
                      <span>{serverSaveError}</span>
                    </div>
                  )}

                  {serverSaveSuccess && (
                    <div className="p-2.5 rounded-lg bg-emerald-50 border border-emerald-200 text-xs text-emerald-700 flex items-center gap-1.5">
                      <CheckCircle2 className="w-4 h-4 shrink-0" />
                      <span>Đã lưu cấu hình máy chủ thành công (rev {formDraft.revision})</span>
                    </div>
                  )}

                  <div className="pt-2 flex justify-end">
                    <button
                      type="submit"
                      disabled={isSavingServer}
                      className="px-4 py-2 text-xs font-semibold bg-emerald-600 text-white rounded-lg hover:bg-emerald-700 transition disabled:opacity-50 cursor-pointer"
                    >
                      {isSavingServer ? 'Đang lưu...' : 'Lưu cấu hình máy chủ'}
                    </button>
                  </div>
                </form>
              )}

              {/* Latency Telemetry Breakdown */}
              {lastTelemetry && (
                <div className="p-2.5 rounded-lg bg-stone-50 border border-stone-200/80 text-xs text-stone-600 space-y-2">
                  <div className="flex items-center justify-between">
                    <span className="font-semibold text-stone-700 block">Độ trễ phát âm gần nhất:</span>
                    <button
                      type="button"
                      onClick={handleDownloadTelemetry}
                      className="text-[11px] font-medium text-stone-600 hover:text-stone-900 bg-white border border-stone-200 px-2 py-0.5 rounded cursor-pointer transition shadow-xs"
                    >
                      Tải JSON đo
                    </button>
                  </div>
                  <div className="grid grid-cols-3 gap-2 text-stone-500 pt-1">
                    <div>
                      <span className="block text-[10px] uppercase text-stone-400">SSE → Chờ hàng đợi</span>
                      <span className="font-mono text-stone-700 font-medium">
                        {lastTelemetry.sseToQueueMs != null ? `${Math.round(lastTelemetry.sseToQueueMs)}ms` : 'N/A'}
                      </span>
                    </div>
                    <div>
                      <span className="block text-[10px] uppercase text-stone-400">Hàng đợi → Bắt đầu</span>
                      <span className="font-mono text-stone-700 font-medium">
                        {lastTelemetry.queueToSpeakMs != null ? `${Math.round(lastTelemetry.queueToSpeakMs)}ms` : 'N/A'}
                      </span>
                    </div>
                    <div>
                      <span className="block text-[10px] uppercase text-stone-400">SSE → Phát âm</span>
                      <span className="font-mono text-stone-700 font-medium">
                        {lastTelemetry.sseToSpeakMs != null ? `${Math.round(lastTelemetry.sseToSpeakMs)}ms` : 'N/A'}
                      </span>
                    </div>
                    <div>
                      <span className="block text-[10px] uppercase text-stone-400">SSE → Đọc số tiền</span>
                      <span className="font-mono text-stone-700 font-medium">
                        {lastTelemetry.sseToAmountMs != null ? `${Math.round(lastTelemetry.sseToAmountMs)}ms` : 'N/A'}
                      </span>
                    </div>
                    <div>
                      <span className="block text-[10px] uppercase text-stone-400">Đường âm thanh</span>
                      <span className="font-mono text-stone-700 font-medium">
                        {lastTelemetry.audioPath ?? 'N/A'}
                      </span>
                    </div>
                    <div>
                      <span className="block text-[10px] uppercase text-stone-400">Kết quả</span>
                      <span className="font-mono text-stone-700 font-medium capitalize">
                        {lastTelemetry.outcome ?? 'completed'}
                      </span>
                    </div>
                  </div>
                  <p className="text-[10px] text-stone-400 italic pt-1">
                    Lưu ý: Không đo được độ trễ ngân hàng từ clock thiết bị.
                  </p>
                </div>
              )}
            </>
          )}

          {/* Modal Actions */}
          <div className="pt-4 border-t border-stone-100 flex items-center justify-between gap-3">
            <VoiceTestButton />
            <button
              type="button"
              onClick={onClose}
              className="px-4 py-2 text-xs font-medium bg-stone-900 text-white rounded-lg hover:bg-stone-800 transition cursor-pointer"
            >
              Đóng
            </button>
          </div>
        </div>
      </div>
    </div>
  );
};
