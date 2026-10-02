import { ApiError } from '../api';

export const ERROR_MESSAGES_MAP: Record<string, string> = {
  SESSION_EXPIRED: 'Phiên ACB đã hết hạn. Mở menu Telegram để đăng nhập lại.',
  ACB_RATE_LIMITED: 'ACB đang giới hạn tần suất truy cập. Hệ thống sẽ tự động thử lại sau ít phút.',
  ORIGIN_MISMATCH: 'Xác thực bảo mật Origin không khớp với cấu hình máy chủ. Vui lòng kiểm tra PUBLIC_ORIGIN.',
  CSRF_TOKEN_INVALID: 'Phiên bảo mật (CSRF) không hợp lệ hoặc đã hết hạn. Vui lòng thử lại.',
  SYNC_UNAVAILABLE: 'Tính năng đồng bộ tức thời chưa sẵn sàng hoặc kết nối ACB chưa kích hoạt.',
};

export function formatErrorMessage(error: unknown): string {
  if (!error) return 'Đã có lỗi xảy ra. Vui lòng thử lại.';

  if (error instanceof ApiError) {
    if (error.code && ERROR_MESSAGES_MAP[error.code]) {
      return ERROR_MESSAGES_MAP[error.code];
    }
    if (error.message && !error.message.includes('<!DOCTYPE')) {
      return error.message;
    }
    return `Máy chủ trả về lỗi HTTP ${error.status}. Vui lòng thử lại.`;
  }

  if (error instanceof Error) {
    if (error.message && !error.message.includes('<!DOCTYPE')) {
      return error.message;
    }
  }

  if (typeof error === 'string') {
    return error;
  }

  return 'Không thể kết nối máy chủ hoặc phản hồi không hợp lệ. Vui lòng thử lại.';
}
