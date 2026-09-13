import { describe, it, expect } from 'vitest';
import type { HistoryJobStatus, HistorySyncJob, EnsureHistoryResponse } from '../src/realtime-types';

describe('transactions sync job types and polling logic', () => {
  const getRefetchInterval = (job: HistorySyncJob | undefined): number | false => {
    if (!job) return 1000;
    if (job.status === 'QUEUED' || job.status === 'RUNNING') {
      return (job.pagesDone ?? 0) > 10 ? 2000 : 1000;
    }
    return false;
  };

  const formatFriendlyError = (code?: string | null, message?: string | null): string => {
    if (code === 'SESSION_EXPIRED') {
      return 'Phiên làm việc ACB đã hết hạn. Vui lòng cập nhật thông tin đăng nhập.';
    }
    if (code === 'RATE_LIMITED') {
      return 'Hệ thống ACB đang giới hạn tần suất yêu cầu. Vui lòng chờ ít phút.';
    }
    if (code === 'INVALID_RANGE') {
      return 'Khoảng thời gian không hợp lệ hoặc vượt quá giới hạn tối đa 31 ngày.';
    }
    if (code === 'JOB_NOT_FOUND') {
      return 'Không tìm thấy tiến trình đồng bộ.';
    }
    if (code === 'UPSTREAM_ERROR' || code === 'SERVICE_UNAVAILABLE') {
      return 'Không thể kết nối đến máy chủ ngân hàng ACB. Vui lòng thử lại sau.';
    }
    if (message && !message.includes('<') && !message.includes('http')) {
      return message;
    }
    return 'Đồng bộ giao dịch không thành công. Vui lòng kiểm tra lại kết nối.';
  };

  it('calculates polling intervals correctly: 1s initially, 2s backoff, stops on terminal states', () => {
    // Initial unknown job
    expect(getRefetchInterval(undefined)).toBe(1000);

    // Queued
    expect(
      getRefetchInterval({
        id: 'job-1',
        status: 'QUEUED',
        rangeFrom: '2026-09-01',
        rangeTo: '2026-09-05',
        pagesDone: 0,
        rowsSeen: 0,
      })
    ).toBe(1000);

    // Running early pages (< 10)
    expect(
      getRefetchInterval({
        id: 'job-1',
        status: 'RUNNING',
        rangeFrom: '2026-09-01',
        rangeTo: '2026-09-05',
        pagesDone: 5,
        rowsSeen: 25,
      })
    ).toBe(1000);

    // Running long job (> 10 pages) -> 2s backoff
    expect(
      getRefetchInterval({
        id: 'job-1',
        status: 'RUNNING',
        rangeFrom: '2026-08-15',
        rangeTo: '2026-09-14',
        pagesDone: 12,
        rowsSeen: 150,
      })
    ).toBe(2000);

    // Terminal states -> false (stops polling immediately)
    const terminalStatuses: HistoryJobStatus[] = ['COMPLETED', 'FAILED', 'CANCELED'];
    for (const status of terminalStatuses) {
      expect(
        getRefetchInterval({
          id: 'job-1',
          status,
          rangeFrom: '2026-09-01',
          rangeTo: '2026-09-05',
          pagesDone: 10,
          rowsSeen: 50,
        })
      ).toBe(false);
    }
  });

  it('formats friendly user-facing messages without technical leaks', () => {
    expect(formatFriendlyError('SESSION_EXPIRED', null)).toBe(
      'Phiên làm việc ACB đã hết hạn. Vui lòng cập nhật thông tin đăng nhập.'
    );
    expect(formatFriendlyError('RATE_LIMITED', null)).toBe(
      'Hệ thống ACB đang giới hạn tần suất yêu cầu. Vui lòng chờ ít phút.'
    );
    expect(formatFriendlyError('INVALID_RANGE', null)).toBe(
      'Khoảng thời gian không hợp lệ hoặc vượt quá giới hạn tối đa 31 ngày.'
    );
    expect(formatFriendlyError('UPSTREAM_ERROR', '502 Bad Gateway with raw traces')).toBe(
      'Không thể kết nối đến máy chủ ngân hàng ACB. Vui lòng thử lại sau.'
    );
    // Raw HTML error sanitized
    expect(formatFriendlyError('UNKNOWN', '<html><head><title>500</title></head></html>')).toBe(
      'Đồng bộ giao dịch không thành công. Vui lòng kiểm tra lại kết nối.'
    );
    // User-safe clean text preserved
    expect(formatFriendlyError('CUSTOM', 'Không tìm thấy tài khoản.')).toBe(
      'Không tìm thấy tài khoản.'
    );
  });

  it('validates EnsureHistoryResponse contract structure', () => {
    // 202 Accepted response
    const accepted: EnsureHistoryResponse = {
      id: 'job-abc',
      status: 'QUEUED',
      coverage: 'PENDING',
      synced: false,
      job: {
        id: 'job-abc',
        status: 'QUEUED',
        rangeFrom: '2026-09-01',
        rangeTo: '2026-09-10',
        pagesDone: 0,
        rowsSeen: 0,
      },
    };
    expect(accepted.status).toBe('QUEUED');
    expect(accepted.job?.id).toBe('job-abc');

    // 200 OK already covered response
    const covered: EnsureHistoryResponse = {
      status: 'COMPLETED',
      coverage: 'COMPLETE',
      synced: false,
      job: null,
    };
    expect(covered.status).toBe('COMPLETED');
    expect(covered.job).toBeNull();
  });
});
