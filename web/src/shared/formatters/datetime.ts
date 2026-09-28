/**
 * Date and time formatting helpers for human-friendly Vietnamese displays.
 * Standard clean format: DD/MM/YYYY HH:mm:ss (or DD/MM/YYYY if date-only).
 * Eliminates raw ISO strings (e.g. 2026-09-28T21:51:36+07:00).
 */

export function formatDateTimeVN(raw?: string | number | Date | null): string {
  if (!raw) return '';
  let d: Date;
  if (raw instanceof Date) {
    d = raw;
  } else if (typeof raw === 'number') {
    d = new Date(raw);
  } else {
    const s = String(raw).trim();
    if (!s) return '';
    // If it's pure DD/MM/YYYY HH:mm:ss or DD/MM/YYYY already
    if (/^\d{2}\/\d{2}\/\d{4}( \d{2}:\d{2}(:\d{2})?)?$/.test(s)) {
      return s;
    }
    d = new Date(s);
  }

  if (isNaN(d.getTime())) {
    return String(raw);
  }

  return new Intl.DateTimeFormat('vi-VN', {
    timeZone: 'Asia/Ho_Chi_Minh',
    day: '2-digit',
    month: '2-digit',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(d);
}

export function formatDateOnlyVN(raw?: string | number | Date | null): string {
  if (!raw) return '';
  let d: Date;
  if (raw instanceof Date) {
    d = raw;
  } else if (typeof raw === 'number') {
    d = new Date(raw);
  } else {
    const s = String(raw).trim();
    if (!s) return '';
    if (/^\d{2}\/\d{2}\/\d{4}$/.test(s)) {
      return s;
    }
    d = new Date(s);
  }

  if (isNaN(d.getTime())) {
    return String(raw);
  }

  return new Intl.DateTimeFormat('vi-VN', {
    timeZone: 'Asia/Ho_Chi_Minh',
    day: '2-digit',
    month: '2-digit',
    year: 'numeric',
  }).format(d);
}

export function formatTimeOnlyVN(raw?: string | number | Date | null): string {
  if (!raw) return '';
  let d: Date;
  if (raw instanceof Date) {
    d = raw;
  } else if (typeof raw === 'number') {
    d = new Date(raw);
  } else {
    const s = String(raw).trim();
    if (!s) return '';
    d = new Date(s);
  }

  if (isNaN(d.getTime())) {
    return String(raw);
  }

  return new Intl.DateTimeFormat('vi-VN', {
    timeZone: 'Asia/Ho_Chi_Minh',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(d);
}
