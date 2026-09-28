import { describe, it, expect } from 'vitest';
import {
  formatDateTimeVN,
  formatDateOnlyVN,
  formatTimeOnlyVN,
} from '../src/shared/formatters/datetime';

describe('datetime formatters', () => {
  it('formats ISO 8601 string to Vietnamese date time DD/MM/YYYY HH:mm:ss', () => {
    const iso = '2026-09-28T21:51:36+07:00';
    const formatted = formatDateTimeVN(iso);
    expect(formatted).toContain('28/09/2026');
    expect(formatted).toContain('21:51:36');
  });

  it('formats UTC ISO timestamp into Asia/Ho_Chi_Minh (+07:00)', () => {
    const utcIso = '2026-09-28T14:51:36Z';
    const formatted = formatDateTimeVN(utcIso);
    expect(formatted).toContain('28/09/2026');
    expect(formatted).toContain('21:51:36');
  });

  it('preserves existing DD/MM/YYYY HH:mm:ss string without distorting', () => {
    const raw = '28/09/2026 14:30:00';
    expect(formatDateTimeVN(raw)).toBe('28/09/2026 14:30:00');
  });

  it('handles empty or null safely', () => {
    expect(formatDateTimeVN(null)).toBe('');
    expect(formatDateTimeVN('')).toBe('');
    expect(formatDateTimeVN(undefined)).toBe('');
  });
});
