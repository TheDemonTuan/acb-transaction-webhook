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

export const realtimeProfile = { mode: 'REALTIME' as const, minSeconds: 20, maxSeconds: 30 };
export const keepaliveProfile = { mode: 'KEEPALIVE_ONLY' as const, minSeconds: 60, maxSeconds: 120 };

export const DAYS_OF_WEEK = [
  { day: 1, label: 'Thứ 2', short: 'T2' },
  { day: 2, label: 'Thứ 3', short: 'T3' },
  { day: 3, label: 'Thứ 4', short: 'T4' },
  { day: 4, label: 'Thứ 5', short: 'T5' },
  { day: 5, label: 'Thứ 6', short: 'T6' },
  { day: 6, label: 'Thứ 7', short: 'T7' },
  { day: 0, label: 'Chủ nhật', short: 'CN' },
];

export const isArraysEqual = (a: number[], b: number[]): boolean => {
  if (a.length !== b.length) return false;
  const sortedA = [...a].sort((x, y) => x - y);
  const sortedB = [...b].sort((x, y) => x - y);
  return sortedA.every((v, i) => v === sortedB[i]);
};

export const formatDays = (days: number[]): string => {
  if (!days || days.length === 0) return 'Chưa chọn ngày';
  if (days.length === 7) return 'Hằng ngày';
  if (isArraysEqual(days, [1, 2, 3, 4, 5])) return 'Thứ 2 – Thứ 6';
  if (isArraysEqual(days, [0, 6])) return 'Cuối tuần (T7, CN)';
  const dayNames: Record<number, string> = {
    1: 'T2',
    2: 'T3',
    3: 'T4',
    4: 'T5',
    5: 'T6',
    6: 'T7',
    0: 'CN',
  };
  const order = [1, 2, 3, 4, 5, 6, 0];
  return order.filter((d) => days.includes(d)).map((d) => dayNames[d]).join(', ');
};

export const getMatchedPreset = (
  settings: MonitorSettings | null
): 'v3_standard' | 'business' | 'realtime_247' | 'custom' => {
  if (!settings || !settings.enabled) return 'custom';

  if (
    settings.windows.length === 0 &&
    settings.defaultProfile.mode === 'REALTIME' &&
    settings.defaultProfile.minSeconds === 20 &&
    settings.defaultProfile.maxSeconds === 30
  ) {
    return 'realtime_247';
  }

  if (
    settings.windows.length === 1 &&
    settings.defaultProfile.mode === 'KEEPALIVE_ONLY' &&
    settings.defaultProfile.minSeconds === 60 &&
    settings.defaultProfile.maxSeconds === 120
  ) {
    const w = settings.windows[0];
    const isProfileRt =
      w.profile.mode === 'REALTIME' &&
      w.profile.minSeconds === 20 &&
      w.profile.maxSeconds === 30;

    if (
      isProfileRt &&
      w.startTime === '07:00' &&
      w.endTime === '23:00' &&
      isArraysEqual(w.daysOfWeek, [0, 1, 2, 3, 4, 5, 6])
    ) {
      return 'v3_standard';
    }

    if (
      isProfileRt &&
      w.startTime === '08:00' &&
      w.endTime === '18:00' &&
      isArraysEqual(w.daysOfWeek, [1, 2, 3, 4, 5])
    ) {
      return 'business';
    }
  }

  return 'custom';
};

export const validateSchedule = (settings: MonitorSettings): string[] => {
  const errors: string[] = [];

  if (settings.defaultProfile.minSeconds > settings.defaultProfile.maxSeconds) {
    errors.push('Cấu hình ngoài khung giờ: Giây tối thiểu không được lớn hơn tối đa.');
  }
  if (settings.defaultProfile.mode === 'REALTIME') {
    if (settings.defaultProfile.minSeconds < 3 || settings.defaultProfile.maxSeconds > 300) {
      errors.push('Cấu hình ngoài khung giờ (REALTIME): Cần từ 3 đến 300 giây.');
    }
  } else if (settings.defaultProfile.mode === 'KEEPALIVE_ONLY') {
    if (settings.defaultProfile.minSeconds < 60 || settings.defaultProfile.maxSeconds > 1800) {
      errors.push('Cấu hình ngoài khung giờ (KEEPALIVE): Cần từ 60 đến 1800 giây.');
    }
  }

  settings.windows.forEach((win, idx) => {
    const prefix = `Khung giờ #${idx + 1} (${win.name || 'Chưa đặt tên'}): `;
    if (!win.startTime || !win.endTime) {
      errors.push(`${prefix}Chưa điền đủ giờ bắt đầu và kết thúc.`);
    } else if (win.startTime === win.endTime) {
      errors.push(`${prefix}Giờ bắt đầu và kết thúc không được trùng nhau.`);
    }

    if (!win.daysOfWeek || win.daysOfWeek.length === 0) {
      errors.push(`${prefix}Phải chọn ít nhất 1 ngày áp dụng.`);
    } else {
      for (const d of win.daysOfWeek) {
        if (d < 0 || d > 6) {
          errors.push(`${prefix}Ngày trong tuần không hợp lệ (0-6).`);
          break;
        }
      }
    }

    if (win.profile.minSeconds > win.profile.maxSeconds) {
      errors.push(`${prefix}Giây tối thiểu không được lớn hơn tối đa.`);
    }
    if (win.profile.mode === 'REALTIME') {
      if (win.profile.minSeconds < 3 || win.profile.maxSeconds > 300) {
        errors.push(`${prefix}Khoảng cách REALTIME cần từ 3 đến 300 giây.`);
      }
    } else if (win.profile.mode === 'KEEPALIVE_ONLY') {
      if (win.profile.minSeconds < 60 || win.profile.maxSeconds > 1800) {
        errors.push(`${prefix}Khoảng cách KEEPALIVE cần từ 60 đến 1800 giây.`);
      }
    }
  });

  return errors;
};

export const ScheduleSettingsSection: React.FC = () => {
  const queryClient = useQueryClient();
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; message: string } | null>(null);

  const { data, isLoading, refetch } = useQuery<MonitorSettingsResponse>({
    queryKey: queryKeys.monitorSettings,
    queryFn: fetchMonitorSettings,
  });

  const [formSettings, setFormSettings] = useState<MonitorSettings | null>(null);
  const [lastSyncedServerJson, setLastSyncedServerJson] = useState<string>('');

  useEffect(() => {
    if (!data?.settings) return;
    const currentJson = JSON.stringify(data.settings);
    // Sync only on initial load or if user hasn't made unsaved edits
    if (!formSettings || JSON.stringify(formSettings) === lastSyncedServerJson) {
      setFormSettings(JSON.parse(currentJson));
      setLastSyncedServerJson(currentJson);
    }
  }, [data?.settings]);

  const isDirty =
    formSettings && data?.settings
      ? JSON.stringify(formSettings) !== JSON.stringify(data.settings)
      : false;

  const validationErrors = formSettings ? validateSchedule(formSettings) : [];
  const matchedPreset = formSettings ? getMatchedPreset(formSettings) : 'custom';

  const saveMutation = useMutation({
    mutationFn: (settings: MonitorSettings) => updateMonitorSettings(settings),
    onSuccess: (resp) => {
      setNotice({ kind: 'success', message: 'Đã lưu cấu hình lịch trình polling thành công!' });
      queryClient.invalidateQueries({ queryKey: queryKeys.monitorSettings });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
      if (resp?.settings) {
        const json = JSON.stringify(resp.settings);
        setFormSettings(JSON.parse(json));
        setLastSyncedServerJson(json);
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

  const handleRefresh = async () => {
    if (isDirty) {
      const ok = window.confirm(
        'Bạn có thay đổi chưa lưu trên form. Tải lại sẽ khôi phục về cấu hình đã lưu trên máy chủ và hủy các thay đổi này. Tiếp tục?'
      );
      if (!ok) return;
    }
    const result = await refetch();
    if (result.data?.settings) {
      const json = JSON.stringify(result.data.settings);
      setFormSettings(JSON.parse(json));
      setLastSyncedServerJson(json);
      setNotice(null);
    }
  };

  if (isLoading || !formSettings) {
    return (
      <div className="p-6 text-center text-xs text-stone-500 font-medium">
        Đang tải cấu hình lịch trình polling...
      </div>
    );
  }

  const current = data?.current;

  const applyPreset = (presetType: 'v3_standard' | 'business' | 'realtime_247') => {
    const updated = JSON.parse(JSON.stringify(formSettings)) as MonitorSettings;
    updated.enabled = true;
    if (presetType === 'v3_standard') {
      updated.defaultProfile = { ...keepaliveProfile };
      updated.windows = [
        {
          name: 'Giờ hoạt động thường ngày',
          daysOfWeek: [0, 1, 2, 3, 4, 5, 6],
          startTime: '07:00',
          endTime: '23:00',
          profile: { ...realtimeProfile },
        },
      ];
    } else if (presetType === 'business') {
      updated.defaultProfile = { ...keepaliveProfile };
      updated.windows = [
        {
          name: 'Giờ hành chính (Thứ 2 - Thứ 6)',
          daysOfWeek: [1, 2, 3, 4, 5],
          startTime: '08:00',
          endTime: '18:00',
          profile: { ...realtimeProfile },
        },
      ];
    } else if (presetType === 'realtime_247') {
      updated.defaultProfile = { ...realtimeProfile };
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
      profile: { ...realtimeProfile },
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

  const toggleDay = (windowIndex: number, day: number) => {
    const win = formSettings.windows[windowIndex];
    const exists = (win.daysOfWeek || []).includes(day);
    const nextDays = exists
      ? win.daysOfWeek.filter((d) => d !== day)
      : [...(win.daysOfWeek || []), day].sort((a, b) => a - b);
    handleWindowChange(windowIndex, { daysOfWeek: nextDays });
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
          onClick={handleRefresh}
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
                <CheckCircle2 className="w-4 h-4 text-emerald-600 shrink-0" />
              ) : (
                <AlertTriangle className="w-4 h-4 text-rose-600 shrink-0" />
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

        {/* Live Status Preview (Running on Server) */}
        {current && (
          <div className="bg-stone-50/80 p-4 rounded-xl border border-stone-200/70 space-y-3">
            <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
              <div className="flex items-center gap-2">
                <span className="text-xs font-semibold text-stone-700">Trạng thái hiện tại:</span>
                <span className="text-[10px] font-medium px-1.5 py-0.5 rounded bg-stone-200 text-stone-700">
                  Đang chạy trên máy chủ
                </span>
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
              Khi tắt, hệ thống sẽ chạy Realtime liên tục 5–15s 24/7 theo cấu hình mặc định của hệ thống
            </p>
          </div>
          <label className="relative inline-flex items-center cursor-pointer">
            <input
              type="checkbox"
              checked={formSettings.enabled}
              onChange={(e) => setFormSettings({ ...formSettings, enabled: e.target.checked })}
              className="sr-only peer"
            />
            <div className="w-11 h-6 bg-stone-200 peer-focus:outline-none rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-stone-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-stone-900"></div>
          </label>
        </div>

        {/* Preset Templates */}
        <div>
          <div className="flex items-center justify-between mb-2">
            <span className="text-xs font-bold text-stone-800">Áp dụng mẫu có sẵn:</span>
            {matchedPreset !== 'custom' ? (
              <span className="text-[11px] font-medium text-emerald-700 bg-emerald-50 px-2 py-0.5 rounded-full border border-emerald-200">
                Đang khớp mẫu: {matchedPreset === 'v3_standard' ? 'Chuẩn V3' : matchedPreset === 'business' ? 'Giờ hành chính' : 'Realtime 24/7'}
              </span>
            ) : (
              <span className="text-[11px] font-medium text-stone-500 bg-stone-100 px-2 py-0.5 rounded-full border border-stone-200">
                Cấu hình tùy chỉnh
              </span>
            )}
          </div>

          <div className="grid grid-cols-1 sm:grid-cols-3 gap-2.5">
            <button
              type="button"
              onClick={() => applyPreset('v3_standard')}
              className={`p-3 text-left rounded-xl border transition cursor-pointer relative ${
                matchedPreset === 'v3_standard'
                  ? 'border-stone-900 bg-stone-50/80 ring-1 ring-stone-900 shadow-2xs'
                  : 'border-stone-200 hover:border-stone-900 bg-stone-50/50 hover:bg-white'
              }`}
            >
              <div className="flex items-center justify-between gap-1.5 font-bold text-xs text-stone-900">
                <div className="flex items-center gap-1.5">
                  <Zap className="w-3.5 h-3.5 text-amber-500" />
                  Chuẩn V3 (Khuyến nghị)
                </div>
                {matchedPreset === 'v3_standard' && (
                  <span className="text-[10px] px-1.5 py-0.5 rounded bg-stone-900 text-white font-medium">
                    Đang khớp
                  </span>
                )}
              </div>
              <p className="text-xs text-stone-500 mt-1">
                07:00–23:00 Realtime (3–10s)<br />23:00–07:00 Giữ phiên (1–2p)
              </p>
            </button>

            <button
              type="button"
              onClick={() => applyPreset('business')}
              className={`p-3 text-left rounded-xl border transition cursor-pointer relative ${
                matchedPreset === 'business'
                  ? 'border-stone-900 bg-stone-50/80 ring-1 ring-stone-900 shadow-2xs'
                  : 'border-stone-200 hover:border-stone-900 bg-stone-50/50 hover:bg-white'
              }`}
            >
              <div className="flex items-center justify-between gap-1.5 font-bold text-xs text-stone-900">
                <div className="flex items-center gap-1.5">
                  <Coffee className="w-3.5 h-3.5 text-blue-500" />
                  Giờ hành chính T2-T6
                </div>
                {matchedPreset === 'business' && (
                  <span className="text-[10px] px-1.5 py-0.5 rounded bg-stone-900 text-white font-medium">
                    Đang khớp
                  </span>
                )}
              </div>
              <p className="text-xs text-stone-500 mt-1">
                08:00–18:00 Realtime (3–10s)<br />Ngoài giờ Giữ phiên (1–2p)
              </p>
            </button>

            <button
              type="button"
              onClick={() => applyPreset('realtime_247')}
              className={`p-3 text-left rounded-xl border transition cursor-pointer relative ${
                matchedPreset === 'realtime_247'
                  ? 'border-stone-900 bg-stone-50/80 ring-1 ring-stone-900 shadow-2xs'
                  : 'border-stone-200 hover:border-stone-900 bg-stone-50/50 hover:bg-white'
              }`}
            >
              <div className="flex items-center justify-between gap-1.5 font-bold text-xs text-stone-900">
                <div className="flex items-center gap-1.5">
                  <PauseCircle className="w-3.5 h-3.5 text-emerald-500" />
                  Realtime 24/7
                </div>
                {matchedPreset === 'realtime_247' && (
                  <span className="text-[10px] px-1.5 py-0.5 rounded bg-stone-900 text-white font-medium">
                    Đang khớp
                  </span>
                )}
              </div>
              <p className="text-xs text-stone-500 mt-1">
                Quét liên tục 3–10s cả ngày<br />(Yêu cầu mạng ổn định)
              </p>
            </button>
          </div>
        </div>

        {/* Draft Summary Block immediately upon preset / edits */}
        <div className="bg-stone-50 p-4 rounded-xl border border-stone-200/80 space-y-2 text-xs">
          <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 border-b border-stone-200/60 pb-2">
            <div className="flex items-center gap-2">
              <span className="font-bold text-stone-800">Tóm tắt bản nháp cấu hình</span>
              {isDirty ? (
                <span className="px-2 py-0.5 rounded-full text-[10px] font-bold bg-amber-100 text-amber-800 border border-amber-200">
                  Bản nháp chưa lưu
                </span>
              ) : (
                <span className="px-2 py-0.5 rounded-full text-[10px] font-bold bg-stone-200 text-stone-700">
                  Đã đồng bộ với máy chủ
                </span>
              )}
            </div>
            <div className="text-[11px] text-stone-500">
              Múi giờ: <strong className="text-stone-700">{formSettings.timezone || 'Asia/Ho_Chi_Minh'}</strong>
            </div>
          </div>

          <div className="text-stone-700 leading-relaxed">
            {!formSettings.enabled ? (
              <p className="text-stone-500 italic">
                Lịch trình thông minh đang tắt. Hệ thống sẽ chạy Realtime liên tục 5–15s 24/7 theo cấu hình mặc định của hệ thống.
              </p>
            ) : matchedPreset === 'v3_standard' ? (
              <p>
                <strong>Mẫu chuẩn:</strong> hằng ngày 07:00–23:00, realtime 3–10 giây; ngoài giờ giữ phiên 1–2 phút.
              </p>
            ) : matchedPreset === 'business' ? (
              <p>
                <strong>Giờ hành chính:</strong> Thứ 2–Thứ 6, 08:00–18:00; ngoài khung giờ và cuối tuần giữ phiên 1–2 phút.
              </p>
            ) : matchedPreset === 'realtime_247' ? (
              <p>
                <strong>Realtime 24/7:</strong> cả tuần, cả ngày; không có khung giờ riêng.
              </p>
            ) : (
              <div className="space-y-1">
                <p className="font-medium">
                  <strong>Tùy chỉnh:</strong> {formSettings.windows.length} khung giờ ưu tiên.
                </p>
                {formSettings.windows.length > 0 ? (
                  <ul className="list-disc list-inside space-y-0.5 text-stone-600 pl-1">
                    {formSettings.windows.map((w, idx) => (
                      <li key={idx}>
                        <span className="font-medium text-stone-800">{w.name || `Khung #${idx + 1}`}:</span> {formatDays(w.daysOfWeek)} ({w.startTime}–{w.endTime}{w.startTime && w.endTime && w.startTime > w.endTime ? ' +1 ngày' : ''}) &rarr; {w.profile.mode} ({w.profile.minSeconds}–{w.profile.maxSeconds}s)
                      </li>
                    ))}
                  </ul>
                ) : (
                  <p className="text-stone-500 italic">Không có khung giờ ưu tiên nào.</p>
                )}
                <p className="text-[11px] text-stone-500 pt-0.5">
                  Ngoài các khung giờ trên: {formSettings.defaultProfile.mode === 'KEEPALIVE_ONLY' ? `Giữ phiên (${formSettings.defaultProfile.minSeconds}–${formSettings.defaultProfile.maxSeconds}s)` : `${formSettings.defaultProfile.mode} (${formSettings.defaultProfile.minSeconds}–${formSettings.defaultProfile.maxSeconds}s)`}
                </p>
              </div>
            )}
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

                  {/* Days of week */}
                  <div className="space-y-1.5 pt-1">
                    <div className="flex flex-wrap items-center justify-between gap-1">
                      <span className="text-stone-600 font-medium">Ngày áp dụng:</span>
                      <div className="flex items-center gap-2 text-[11px] text-stone-500">
                        <button
                          type="button"
                          onClick={() => handleWindowChange(idx, { daysOfWeek: [0, 1, 2, 3, 4, 5, 6] })}
                          className="hover:text-stone-900 underline cursor-pointer"
                        >
                          Cả tuần
                        </button>
                        <span>•</span>
                        <button
                          type="button"
                          onClick={() => handleWindowChange(idx, { daysOfWeek: [1, 2, 3, 4, 5] })}
                          className="hover:text-stone-900 underline cursor-pointer"
                        >
                          Thứ 2–Thứ 6
                        </button>
                        <span>•</span>
                        <button
                          type="button"
                          onClick={() => handleWindowChange(idx, { daysOfWeek: [0, 6] })}
                          className="hover:text-stone-900 underline cursor-pointer"
                        >
                          Cuối tuần
                        </button>
                      </div>
                    </div>

                    <div className="flex flex-wrap items-center gap-1.5">
                      {DAYS_OF_WEEK.map((d) => {
                        const isSelected = (win.daysOfWeek || []).includes(d.day);
                        return (
                          <button
                            key={d.day}
                            type="button"
                            aria-label={d.label}
                            aria-pressed={isSelected}
                            onClick={() => toggleDay(idx, d.day)}
                            className={`px-2.5 py-1 rounded-lg text-xs font-semibold transition cursor-pointer ${
                              isSelected
                                ? 'bg-stone-900 text-white shadow-2xs'
                                : 'bg-white text-stone-600 border border-stone-200 hover:bg-stone-100'
                            }`}
                            title={d.label}
                          >
                            {d.short}
                          </button>
                        );
                      })}
                      {(!win.daysOfWeek || win.daysOfWeek.length === 0) && (
                        <span className="text-[11px] text-rose-600 font-semibold ml-1">
                          ⚠️ Chọn ít nhất 1 ngày
                        </span>
                      )}
                    </div>
                  </div>

                  {/* Time & Mode Controls */}
                  <div className="grid grid-cols-1 sm:grid-cols-4 gap-3">
                    <div>
                      <div className="flex items-center justify-between mb-1">
                        <span className="text-stone-500">Bắt đầu (HH:MM):</span>
                      </div>
                      <input
                        type="time"
                        value={win.startTime}
                        onChange={(e) => handleWindowChange(idx, { startTime: e.target.value })}
                        className="w-full px-2.5 py-1.5 bg-white border border-stone-200 rounded-lg text-xs font-mono"
                      />
                    </div>

                    <div>
                      <div className="flex items-center justify-between mb-1">
                        <span className="text-stone-500">Kết thúc (HH:MM):</span>
                        {win.startTime && win.endTime && win.startTime > win.endTime && (
                          <span className="text-[10px] text-amber-700 bg-amber-50 px-1 py-0.2 rounded font-medium border border-amber-200">
                            +1 ngày
                          </span>
                        )}
                      </div>
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
                        className="w-full px-2.5 py-1.5 bg-white border border-stone-200 rounded-lg text-xs"
                      >
                        <option value="REALTIME">REALTIME (Quét liên tục)</option>
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

                  {win.startTime && win.endTime && win.startTime > win.endTime && (
                    <p className="text-[11px] text-amber-700 font-medium">
                      ℹ️ Khung giờ qua đêm: Bắt đầu lúc {win.startTime} và kết thúc vào {win.endTime} sáng ngày hôm sau.
                    </p>
                  )}
                  {win.startTime && win.endTime && win.startTime === win.endTime && (
                    <p className="text-[11px] text-rose-600 font-medium">
                      ⚠️ Giờ bắt đầu và kết thúc không được trùng nhau.
                    </p>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>

        {/* Validation Errors Notice */}
        {validationErrors.length > 0 && (
          <div className="p-3.5 rounded-xl text-xs bg-rose-50 text-rose-800 border border-rose-200 space-y-1">
            <div className="flex items-center gap-1.5 font-bold">
              <AlertTriangle className="w-4 h-4 text-rose-600 shrink-0" />
              Cần sửa các lỗi sau trước khi lưu:
            </div>
            <ul className="list-disc list-inside space-y-0.5 text-rose-700 text-[11px] pl-1">
              {validationErrors.map((err, i) => (
                <li key={i}>{err}</li>
              ))}
            </ul>
          </div>
        )}

        {/* Save button */}
        <div className="pt-3 border-t border-stone-100 flex flex-col sm:flex-row sm:items-center justify-between gap-3">
          <div className="text-xs text-stone-500">
            {isDirty ? (
              <span className="text-amber-700 font-medium flex items-center gap-1">
                <span className="w-1.5 h-1.5 rounded-full bg-amber-500" />
                Có thay đổi chưa lưu
              </span>
            ) : (
              <span className="text-stone-500 flex items-center gap-1">
                <span className="w-1.5 h-1.5 rounded-full bg-stone-400" />
                Cấu hình khớp với máy chủ
              </span>
            )}
          </div>

          <button
            type="button"
            onClick={() => {
              if (validationErrors.length > 0) {
                setNotice({ kind: 'error', message: validationErrors[0] });
                return;
              }
              saveMutation.mutate(formSettings);
            }}
            disabled={saveMutation.isPending || validationErrors.length > 0}
            className="inline-flex items-center justify-center gap-2 px-5 py-2.5 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 shadow-2xs transition disabled:opacity-50 cursor-pointer"
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
