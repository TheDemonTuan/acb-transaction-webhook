export type Tone = 'success' | 'warning' | 'danger' | 'neutral' | 'info';

export interface StatusDescriptor {
  label: string;
  badge: string;
  tone: Tone;
  description?: string;
}

export const ACB_STATUS_MAP: Record<string, StatusDescriptor> = {
  UNCONFIGURED: {
    label: 'Chưa kết nối ngân hàng',
    badge: 'Chưa kết nối',
    tone: 'neutral',
    description: 'Vui lòng cấu hình số tài khoản và đăng nhập ACB để bắt đầu nhận giao dịch.',
  },
  AUTH_REQUIRED: {
    label: 'Cần đăng nhập lại ACB',
    badge: 'Cần xác thực',
    tone: 'warning',
    description: 'Phiên đăng nhập ngân hàng đã hết hạn hoặc cần xác thực lại để tiếp tục nhận giao dịch.',
  },
  MONITORING: {
    label: 'Đang cập nhật giao dịch',
    badge: 'Đang hoạt động',
    tone: 'success',
    description: 'Hệ thống đang theo dõi và tự động cập nhật giao dịch mới từ ACB.',
  },
  PAUSED: {
    label: 'Đang tạm dừng',
    badge: 'Tạm dừng',
    tone: 'neutral',
    description: 'Đang tạm dừng cập nhật giao dịch từ ngân hàng.',
  },
  STOPPED: {
    label: 'Đã dừng',
    badge: 'Đã dừng',
    tone: 'danger',
    description: 'Dịch vụ kết nối ngân hàng đã dừng hoạt động.',
  },
  ERROR: {
    label: 'Gặp sự cố kết nối',
    badge: 'Lỗi',
    tone: 'danger',
    description: 'Không thể kết nối đến hệ thống ACB.',
  },
};

export const SERVICE_STATUS_MAP: Record<string, { label: string; tone: Tone }> = {
  HEALTHY: { label: 'Hệ thống hoạt động bình thường', tone: 'success' },
  READY: { label: 'Sẵn sàng', tone: 'success' },
  DEGRADED: { label: 'Hoạt động hạn chế', tone: 'warning' },
  UNHEALTHY: { label: 'Gặp sự cố', tone: 'danger' },
};

export const WEBHOOK_STATUS_MAP: Record<string, { label: string; tone: Tone }> = {
  ACTIVE: { label: 'Đang hoạt động', tone: 'success' },
  DISABLED: { label: 'Đang tắt', tone: 'neutral' },
};

export const NOTIFICATION_CHANNEL_STATUS_MAP: Record<string, { label: string; tone: Tone }> = {
  ACTIVE: { label: 'Đang hoạt động', tone: 'success' },
  DISABLED: { label: 'Đang tắt', tone: 'neutral' },
};

export const DELIVERY_STATUS_MAP: Record<string, { label: string; tone: Tone }> = {
  DELIVERED: { label: 'Thành công', tone: 'success' },
  SUCCESS: { label: 'Thành công', tone: 'success' },
  PENDING: { label: 'Đang gửi', tone: 'neutral' },
  IN_FLIGHT: { label: 'Đang xử lý', tone: 'neutral' },
  RETRYING: { label: 'Đang gửi lại', tone: 'warning' },
  FAILED: { label: 'Thất bại', tone: 'danger' },
  DEAD_LETTER: { label: 'Không gửi được', tone: 'danger' },
};

export const COVERAGE_STATUS_MAP: Record<string, string> = {
  FULL: 'Đầy đủ',
  PARTIAL: 'Một phần',
  NOT_STARTED: 'Chưa bắt đầu',
};

export const POLL_STATUS_MAP: Record<string, { label: string; tone: Tone }> = {
  SUCCEEDED: { label: 'Thành công', tone: 'success' },
  FAILED: { label: 'Thất bại', tone: 'danger' },
  AUTH_REQUIRED: { label: 'Cần xác thực ACB', tone: 'warning' },
  PROTOCOL_CHANGED: { label: 'Giao thức ACB thay đổi', tone: 'warning' },
  PARTIAL: { label: 'Đồng bộ một phần', tone: 'warning' },
};

export function getPollStatus(status?: string): { label: string; tone: Tone } {
  const normalized = (status || '').toUpperCase();
  return POLL_STATUS_MAP[normalized] || { label: status || 'Không rõ', tone: 'neutral' };
}

export function getAcbStatusDescriptor(state?: string): StatusDescriptor {
  const normalized = (state || 'UNCONFIGURED').toUpperCase();
  return (
    ACB_STATUS_MAP[normalized] || {
      label: state || 'Chưa xác định',
      badge: state || 'Chưa xác định',
      tone: 'neutral',
    }
  );
}

export function getServiceStatus(status?: string): { label: string; tone: Tone } {
  const normalized = (status || '').toUpperCase();
  return SERVICE_STATUS_MAP[normalized] || { label: status || 'Không rõ', tone: 'neutral' };
}

export function getWebhookStatus(status?: string): { label: string; tone: Tone } {
  const normalized = (status || '').toUpperCase();
  return WEBHOOK_STATUS_MAP[normalized] || { label: status || 'Không rõ', tone: 'neutral' };
}

export function getNotificationChannelStatus(status?: string): { label: string; tone: Tone } {
  const normalized = (status || '').toUpperCase();
  return NOTIFICATION_CHANNEL_STATUS_MAP[normalized] || { label: status || 'Không rõ', tone: 'neutral' };
}

export function getDeliveryStatus(status?: string): { label: string; tone: Tone } {
  const normalized = (status || '').toUpperCase();
  return DELIVERY_STATUS_MAP[normalized] || { label: status || 'Không rõ', tone: 'neutral' };
}
