import { describe, it, expect } from 'vitest';
import {
  realtimeProfile,
  keepaliveProfile,
  getMatchedPreset,
  formatDays,
  validateSchedule,
  DAYS_OF_WEEK,
} from '../src/features/bank-connection/ScheduleSettingsSection';
import type { MonitorSettings } from '../src/realtime-types';

describe('Schedule Settings Section Helpers', () => {
  it('defines 60–120s keepalive profile default', () => {
    expect(keepaliveProfile.mode).toBe('KEEPALIVE_ONLY');
    expect(keepaliveProfile.minSeconds).toBe(60);
    expect(keepaliveProfile.maxSeconds).toBe(120);
    expect(realtimeProfile.mode).toBe('REALTIME');
    expect(realtimeProfile.minSeconds).toBe(20);
    expect(realtimeProfile.maxSeconds).toBe(30);
  });

  it('contains correct days of week definitions', () => {
    expect(DAYS_OF_WEEK).toHaveLength(7);
    expect(DAYS_OF_WEEK[0]).toEqual({ day: 1, label: 'Thứ 2', short: 'T2' });
    expect(DAYS_OF_WEEK[6]).toEqual({ day: 0, label: 'Chủ nhật', short: 'CN' });
  });

  it('formats day strings accurately', () => {
    expect(formatDays([0, 1, 2, 3, 4, 5, 6])).toBe('Hằng ngày');
    expect(formatDays([1, 2, 3, 4, 5])).toBe('Thứ 2 – Thứ 6');
    expect(formatDays([0, 6])).toBe('Cuối tuần (T7, CN)');
    expect(formatDays([1, 3, 5])).toBe('T2, T4, T6');
    expect(formatDays([])).toBe('Chưa chọn ngày');
  });

  it('detects presets correctly', () => {
    const v3Standard: MonitorSettings = {
      revision: 1,
      enabled: true,
      timezone: 'Asia/Ho_Chi_Minh',
      defaultProfile: keepaliveProfile,
      windows: [
        {
          name: 'Giờ hoạt động thường ngày',
          daysOfWeek: [0, 1, 2, 3, 4, 5, 6],
          startTime: '07:00',
          endTime: '23:00',
          profile: realtimeProfile,
        },
      ],
    };
    expect(getMatchedPreset(v3Standard)).toBe('v3_standard');

    const business: MonitorSettings = {
      revision: 1,
      enabled: true,
      timezone: 'Asia/Ho_Chi_Minh',
      defaultProfile: keepaliveProfile,
      windows: [
        {
          name: 'Giờ hành chính (Thứ 2 - Thứ 6)',
          daysOfWeek: [1, 2, 3, 4, 5],
          startTime: '08:00',
          endTime: '18:00',
          profile: realtimeProfile,
        },
      ],
    };
    expect(getMatchedPreset(business)).toBe('business');

    const realtime247: MonitorSettings = {
      revision: 1,
      enabled: true,
      timezone: 'Asia/Ho_Chi_Minh',
      defaultProfile: realtimeProfile,
      windows: [],
    };
    expect(getMatchedPreset(realtime247)).toBe('realtime_247');

    const disabled: MonitorSettings = {
      ...v3Standard,
      enabled: false,
    };
    expect(getMatchedPreset(disabled)).toBe('custom');

    const custom: MonitorSettings = {
      ...v3Standard,
      windows: [
        {
          ...v3Standard.windows[0],
          startTime: '09:00',
        },
      ],
    };
    expect(getMatchedPreset(custom)).toBe('custom');
  });

  it('validates schedule settings correctly', () => {
    const validSettings: MonitorSettings = {
      revision: 1,
      enabled: true,
      timezone: 'Asia/Ho_Chi_Minh',
      defaultProfile: { mode: 'KEEPALIVE_ONLY', minSeconds: 60, maxSeconds: 120 },
      windows: [
        {
          name: 'Ca ngày',
          daysOfWeek: [1, 2, 3, 4, 5],
          startTime: '08:00',
          endTime: '17:00',
          profile: { mode: 'REALTIME', minSeconds: 20, maxSeconds: 30 },
        },
      ],
    };
    expect(validateSchedule(validSettings)).toEqual([]);

    // Window with empty daysOfWeek
    const emptyDays: MonitorSettings = {
      ...validSettings,
      windows: [{ ...validSettings.windows[0], daysOfWeek: [] }],
    };
    const emptyDaysErr = validateSchedule(emptyDays);
    expect(emptyDaysErr.some((e) => e.includes('Phải chọn ít nhất 1 ngày'))).toBe(true);

    // Window with start == end time
    const sameTime: MonitorSettings = {
      ...validSettings,
      windows: [{ ...validSettings.windows[0], startTime: '08:00', endTime: '08:00' }],
    };
    const sameTimeErr = validateSchedule(sameTime);
    expect(sameTimeErr.some((e) => e.includes('không được trùng nhau'))).toBe(true);

    // Profile minSeconds > maxSeconds
    const invalidProfile: MonitorSettings = {
      ...validSettings,
      windows: [
        {
          ...validSettings.windows[0],
          profile: { mode: 'REALTIME', minSeconds: 20, maxSeconds: 10 },
        },
      ],
    };
    const profileErr = validateSchedule(invalidProfile);
    expect(profileErr.some((e) => e.includes('Giây tối thiểu không được lớn hơn tối đa'))).toBe(true);

    // Profile out of bounds
    const outOfBounds: MonitorSettings = {
      ...validSettings,
      windows: [
        {
          ...validSettings.windows[0],
          profile: { mode: 'REALTIME', minSeconds: 1, maxSeconds: 10 },
        },
      ],
    };
    const oobErr = validateSchedule(outOfBounds);
    expect(oobErr.some((e) => e.includes('Khoảng cách REALTIME cần từ 3 đến 300 giây'))).toBe(true);
  });
});
