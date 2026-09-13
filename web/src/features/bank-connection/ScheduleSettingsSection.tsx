import React, { useEffect, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import {
  Clock,
  Save,
  CheckCircle2,
  AlertTriangle,
  Plus,
  Trash2,
  Zap,
  Coffee,
  PauseCircle,
  RefreshCw,
} from 'lucide-react';
import { fetchMonitorSettings, updateMonitorSettings } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import type { MonitorSettings, MonitorSettingsResponse, Window } from '../../realtime-types';

export const ScheduleSettingsSection: React.FC = () => {
  const queryClient = useQueryClient();
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; message: string } | null>(null);

  const { data, isLoading, refetch } = useQuery<MonitorSettingsResponse>({
    queryKey: queryKeys.monitorSettings,
    queryFn: fetchMonitorSettings,
  });

  const [formSettings, setFormSettings] = useState<MonitorSettings | null>(null);

  useEffect(() => {
    if (data?.settings) {
      setFormSettings(JSON.parse(JSON.stringify(data.settings)));
    }
  }, [data?.settings]);

  const saveMutation = useMutation({
    mutationFn: (settings: MonitorSettings) => updateMonitorSettings(settings),
    onSuccess: (resp) => {
      setNotice({ kind: 'success', message: 'Đã lưu cấu hình lịch trình polling thành công!' });
      queryClient.invalidateQueries({ queryKey: queryKeys.monitorSettings });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
      if (resp?.settings) {
        setFormSettings(JSON.parse(JSON.stringify(resp.settings)));
      }
    },
    onError: (err: any) => {
      setNotice({
        kind: 'error',
        message: err.message?.includes('conflict')
          ? 'Xung đột phiên: Cấu hình vừa bị thay đổi bởi người dùng khác. Vui lòng bấm làm mới để tải lại!'
          : `Lưu thất bại: ${err.message || 'Lỗi không xác định'}`,
      });
    },
  });

  if (isLoading || !formSettings) {
    return (
      <div className="bg-white p-6 rounded-2xl border border-stone-200/80 shadow-xs text-center text-stone-500 text-xs py-8">
        <RefreshCw className="w-5 h-5 animate-spin mx-auto mb-2 text-stone-400" />
        Đang tải cấu hình lịch trình polling...
      </div>
    );
  }

  const current = data?.current;

  // Apply Presets
  const realtimeProfile = { mode: 'REALTIME' as const, minSeconds: 3, maxSeconds: 10 };
const keepaliveProfile = { mode: 'KEEPALIVE_ONLY' as const, minSeconds: 120, maxSeconds: 180 };

const applyPreset = (presetType: 'v3_standard' | 'business' | 'realtime_247') => {
    const updated = JSON.parse(JSON.stringify(formSettings)) as MonitorSettings;
    updated.enabled = true;
    if (presetType === 'v3_standard') {
      updated.defaultProfile = keepaliveProfile;
      updated.windows = [
        {
          name: 'Giờ hoạt động thường ngày',
          daysOfWeek: [0, 1, 2, 3, 4, 5, 6],
          startTime: '07:00',
          endTime: '23:00',
          profile: realtimeProfile,
        },
      ];
    } else if (presetType === 'business') {
      updated.defaultProfile = keepaliveProfile;
      updated.windows = [
        {
          name: 'Giờ hành chính (Thứ 2 - Thứ 6)',
          daysOfWeek: [1, 2, 3, 4, 5],
          startTime: '08:00',
          endTime: '18:00',
          profile: realtimeProfile,
        },
      ];
    } else if (presetType === 'realtime_247') {
      updated.defaultProfile = realtimeProfile;
      updated.windows = [];
    }
    setFormSettings(updated);
    setNotice({ kind: 'success', message: 'Đã áp dụng mẫu cấu hình. Bấm "Lưu thay đổi" để kích hoạt!' });
  };

  const handleAddWindow = () => {
    const newWindow: Window = {
      name: `Khung giờ mới #${formSettings.windows.length + 1}`,
      daysOfWeek: [0, 1, 2, 3, 4, 5, 6],
      startTime: '08:00',
      endTime: '17:00',
      profile: realtimeProfile,
    };
    setFormSettings({
      ...formSettings,
      windows: [...formSettings.windows, newWindow],
    });
  };

  const handleRemoveWindow = (index: number) => {
    const next = [...formSettings.windows];
    next.splice(index, 1);
    setFormSettings({ ...formSettings, windows: next });
  };

  const handleWindowChange = (index: number, patch: Partial<Window>) => {
    const next = [...formSettings.windows];
    next[index] = { ...next[index], ...patch };
    setFormSettings({ ...formSettings, windows: next });
  };

  return (
    <div className="bg-white rounded-2xl border border-stone-200/80 shadow-xs overflow-hidden">
      {/* Header */}
      <div className="p-6 border-b border-stone-100 flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <div className="flex items-center gap-2">
            <Clock className="w-5 h-5 text-stone-700" />
            <h3 className="text-base font-bold text-stone-900">Lịch trình quét ACB & Giữ phiên (Schedule Polling)</h3>
          </div>
          <p className="text-xs text-stone-500 mt-1">
            Tối ưu hóa tải: Realtime trong khung giờ cần thiết, tự động chuyển sang giữ phiên (KEEPALIVE) ngoài giờ để tránh bị ACB chặn
          </p>
        </div>

        <button
          type="button"
          onClick={() => refetch()}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-semibold bg-stone-50 border border-stone-200 text-stone-600 hover:bg-stone-100 transition cursor-pointer self-start sm:self-auto"
        >
          <RefreshCw className="w-3.5 h-3.5" />
          Làm mới
        </button>
      </div>

      <div className="p-6 space-y-6">
        {notice && (
          <div
            className={`p-4 rounded-xl text-xs font-medium flex items-center justify-between gap-2 ${
              notice.kind === 'success'
                ? 'bg-emerald-50 text-emerald-800 border border-emerald-200'
                : 'bg-rose-50 text-rose-800 border border-rose-200'
            }`}
          >
            <div className="flex items-center gap-2">
              {notice.kind === 'success' ? (
                <CheckCircle2 className="w-4 h-4 shrink-0 text-emerald-600" />
              ) : (
                <AlertTriangle className="w-4 h-4 shrink-0 text-rose-600" />
              )}
              <span>{notice.message}</span>
            </div>
            <button
              type="button"
              onClick={() => setNotice(null)}
              className="text-stone-400 hover:text-stone-600 font-bold"
            >
              &times;
            </button>
          </div>
        )}

        {/* Live Status Preview */}
        {current && (
          <div className="bg-stone-50/80 p-4 rounded-xl border border-stone-200/70 space-y-3">
            <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
              <div className="flex items-center gap-2">
                <span className="text-xs font-semibold text-stone-700">Trạng thái hiện tại:</span>
                {current.mode === 'REALTIME' ? (
                  <span className="inline-flex items-center gap-1.5 px-2.5 py-0.5 rounded-full text-xs font-bold bg-emerald-100 text-emerald-800 border border-emerald-200">
                    <span className="w-2 h-2 rounded-full bg-emerald-500 animate-pulse" />
                    REALTIME (Quét nhanh {current.minSeconds}–{current.maxSeconds}s)
                  </span>
                ) : current.mode === 'KEEPALIVE_ONLY' ? (
                  <span className="inline-flex items-center gap-1.5 px-2.5 py-0.5 rounded-full text-xs font-bold bg-amber-100 text-amber-800 border border-amber-200">
                    <span className="w-2 h-2 rounded-full bg-amber-500" />
                    GIỮ PHIÊN ({Math.round(current.minSeconds / 60)}–{Math.round(current.maxSeconds / 60)} phút)
                  </span>
                ) : (
                  <span className="inline-flex items-center gap-1.5 px-2.5 py-0.5 rounded-full text-xs font-bold bg-stone-200 text-stone-700">
                    <span className="w-2 h-2 rounded-full bg-stone-500" />
                    TẠM DỪNG (PAUSED)
                  </span>
                )}
              </div>

              <span className="text-xs text-stone-500 font-medium">
                Khung giờ: <strong className="text-stone-700">{current.activeWindow || 'Mặc định'}</strong>
              </span>
            </div>

            <div className="text-xs text-stone-600 flex items-center gap-1.5 pt-1 border-t border-stone-200/50">
              <Clock className="w-3.5 h-3.5 text-stone-400 shrink-0" />
              <span>
                Lần chuyển đổi tiếp theo:{' '}
                <strong>
                  {new Date(current.nextTransitionAt).toLocaleTimeString('vi-VN', {
                    hour: '2-digit',
                    minute: '2-digit',
                  })}{' '}
                  ngày{' '}
                  {new Date(current.nextTransitionAt).toLocaleDateString('vi-VN', {
                    day: '2-digit',
                    month: '2-digit',
                  })}
                </strong>{' '}
                &rarr; chuyển sang <strong>{current.nextMode}</strong>
              </span>
            </div>
          </div>
        )}

        {/* Enable Switch */}
        <div className="flex items-center justify-between py-2 border-b border-stone-100">
          <div>
            <p className="text-xs font-bold text-stone-900">Kích hoạt lịch trình thông minh</p>
            <p className="text-xs text-stone-500 mt-0.5">
              Khi tắt, hệ thống sẽ chạy Realtime liên tục 3–10s 24/7 theo cơ chế cũ
            </p>
          </div>
          <label className="relative inline-flex items-center cursor-pointer">
            <input
              type="checkbox"
              checked={formSettings.enabled}
              onChange={(e) => setFormSettings({ ...formSettings, enabled: e.target.checked })}
              className="sr-only peer"
            />
            <div className="w-11 h-6 bg-stone-200 peer-focus:outline-none rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-stone-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-stone-900" />
          </label>
        </div>

        {/* Preset Templates */}
        <div>
          <span className="text-xs font-bold text-stone-800 block mb-2">Áp dụng mẫu có sẵn:</span>
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-2.5">
            <button
              type="button"
              onClick={() => applyPreset('v3_standard')}
              className="p-3 text-left rounded-xl border border-stone-200 hover:border-stone-900 bg-stone-50/50 hover:bg-white transition cursor-pointer"
            >
              <div className="flex items-center gap-1.5 font-bold text-xs text-stone-900">
                <Zap className="w-3.5 h-3.5 text-amber-500" />
                Chuẩn V3 (Khuyến nghị)
              </div>
              <p className="text-xs text-stone-500 mt-1">
                07:00–23:00 Realtime (3–10s)<br />23:00–07:00 Giữ phiên (2–3p)
              </p>
            </button>

            <button
              type="button"
              onClick={() => applyPreset('business')}
              className="p-3 text-left rounded-xl border border-stone-200 hover:border-stone-900 bg-stone-50/50 hover:bg-white transition cursor-pointer"
            >
              <div className="flex items-center gap-1.5 font-bold text-xs text-stone-900">
                <Coffee className="w-3.5 h-3.5 text-blue-500" />
                Giờ hành chính T2-T6
              </div>
              <p className="text-xs text-stone-500 mt-1">
                08:00–18:00 Realtime (3–10s)<br />Ngoài giờ Giữ phiên (2–3p)
              </p>
            </button>

            <button
              type="button"
              onClick={() => applyPreset('realtime_247')}
              className="p-3 text-left rounded-xl border border-stone-200 hover:border-stone-900 bg-stone-50/50 hover:bg-white transition cursor-pointer"
            >
              <div className="flex items-center gap-1.5 font-bold text-xs text-stone-900">
                <PauseCircle className="w-3.5 h-3.5 text-emerald-500" />
                Realtime 24/7
              </div>
              <p className="text-xs text-stone-500 mt-1">
                Quét liên tục 3–10s cả ngày<br />(Yêu cầu mạng ổn định)
              </p>
            </button>
          </div>
        </div>

        {/* Active Windows Configuration */}
        <div className="space-y-3">
          <div className="flex items-center justify-between">
            <span className="text-xs font-bold text-stone-800">Các khung giờ quét ưu tiên:</span>
            <button
              type="button"
              onClick={handleAddWindow}
              className="inline-flex items-center gap-1 px-2.5 py-1 rounded-lg text-xs font-semibold bg-stone-100 hover:bg-stone-200 text-stone-700 transition cursor-pointer"
            >
              <Plus className="w-3.5 h-3.5" />
              Thêm khung giờ
            </button>
          </div>

          {formSettings.windows.length === 0 ? (
            <p className="text-xs text-stone-500 italic py-2">
              Chưa có khung giờ ưu tiên nào. Hệ thống sẽ áp dụng cấu hình mặc định (KEEPALIVE_ONLY hoặc REALTIME 24/7).
            </p>
          ) : (
            <div className="space-y-3">
              {formSettings.windows.map((win, idx) => (
                <div
                  key={idx}
                  className="p-4 rounded-xl border border-stone-200 bg-stone-50/40 space-y-3 text-xs"
                >
                  <div className="flex items-center justify-between gap-2">
                    <input
                      type="text"
                      value={win.name}
                      onChange={(e) => handleWindowChange(idx, { name: e.target.value })}
                      placeholder="Tên khung giờ (ví dụ: Ban ngày)"
                      className="font-bold text-xs bg-transparent border-b border-stone-300 focus:border-stone-900 focus:outline-none py-0.5 px-1 flex-1 text-stone-800"
                    />
                    <button
                      type="button"
                      onClick={() => handleRemoveWindow(idx)}
                      className="text-stone-400 hover:text-rose-600 transition p-1 cursor-pointer"
                      title="Xóa khung giờ này"
                    >
                      <Trash2 className="w-4 h-4" />
                    </button>
                  </div>

                  <div className="grid grid-cols-1 sm:grid-cols-4 gap-3">
                    <div>
                      <span className="text-stone-500 block mb-1">Bắt đầu (HH:MM):</span>
                      <input
                        type="time"
                        value={win.startTime}
                        onChange={(e) => handleWindowChange(idx, { startTime: e.target.value })}
                        className="w-full px-2.5 py-1.5 bg-white border border-stone-200 rounded-lg text-xs font-mono"
                      />
                    </div>

                    <div>
                      <span className="text-stone-500 block mb-1">Kết thúc (HH:MM):</span>
                      <input
                        type="time"
                        value={win.endTime}
                        onChange={(e) => handleWindowChange(idx, { endTime: e.target.value })}
                        className="w-full px-2.5 py-1.5 bg-white border border-stone-200 rounded-lg text-xs font-mono"
                      />
                    </div>

                    <div>
                      <span className="text-stone-500 block mb-1">Chế độ:</span>
                      <select
                        value={win.profile.mode}
                        onChange={(e) =>
                          handleWindowChange(idx, {
                            profile: { ...win.profile, mode: e.target.value as any },
                          })
                        }
                        className="w-full px-2.5 py-1.5 bg-white border border-stone-200 rounded-lg text-xs font-medium"
                      >
                        <option value="REALTIME">REALTIME (Quét biến động)</option>
                        <option value="KEEPALIVE_ONLY">KEEPALIVE_ONLY (Giữ phiên)</option>
                        <option value="PAUSED">PAUSED (Tạm dừng)</option>
                      </select>
                    </div>

                    <div>
                      <span className="text-stone-500 block mb-1">Khoảng cách (giây):</span>
                      <div className="flex items-center gap-1">
                        <input
                          type="number"
                          value={win.profile.minSeconds}
                          onChange={(e) =>
                            handleWindowChange(idx, {
                              profile: { ...win.profile, minSeconds: Number(e.target.value) },
                            })
                          }
                          min={win.profile.mode === 'REALTIME' ? 3 : 60}
                          className="w-16 px-2 py-1.5 bg-white border border-stone-200 rounded-lg text-xs font-mono"
                        />
                        <span className="text-stone-400">–</span>
                        <input
                          type="number"
                          value={win.profile.maxSeconds}
                          onChange={(e) =>
                            handleWindowChange(idx, {
                              profile: { ...win.profile, maxSeconds: Number(e.target.value) },
                            })
                          }
                          min={win.profile.mode === 'REALTIME' ? 3 : 60}
                          className="w-16 px-2 py-1.5 bg-white border border-stone-200 rounded-lg text-xs font-mono"
                        />
                      </div>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>

        {/* Save button */}
        <div className="pt-3 border-t border-stone-100 flex items-center justify-end gap-3">
          <button
            type="button"
            onClick={() => saveMutation.mutate(formSettings)}
            disabled={saveMutation.isPending}
            className="inline-flex items-center gap-2 px-5 py-2.5 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 shadow-2xs transition disabled:opacity-50 cursor-pointer"
          >
            {saveMutation.isPending ? (
              <RefreshCw className="w-3.5 h-3.5 animate-spin" />
            ) : (
              <Save className="w-3.5 h-3.5" />
            )}
            Lưu thay đổi lịch trình
          </button>
        </div>
      </div>
    </div>
  );
};
