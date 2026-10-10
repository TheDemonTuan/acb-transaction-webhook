export type Tone = 'success' | 'warning' | 'danger' | 'neutral' | 'info';

export interface StatusDescriptor {
  label: string;
  badge: string;
  tone: Tone;
  description?: string;
}

export const PAYMENT_STATUS_MAP: Record<string, StatusDescriptor> = {
  READY: { label: 'Sẵn sàng nhận đơn payOS', badge: 'Sẵn sàng', tone: 'success', description: 'Nhận tiền qua KienlongBank theo từng đơn; webhook và đối soát xác nhận thanh toán.' },
  DISABLED: { label: 'Đang tắt nhận đơn mới', badge: 'Tạm dừng', tone: 'neutral', description: 'Đơn đã phát hành vẫn được xử lý và đối soát; không phát QR thay thế.' },
  UNCONFIGURED: { label: 'Chưa cấu hình đủ khóa payOS', badge: 'Chưa cấu hình', tone: 'warning', description: 'Owner nhập và lưu bộ khóa của cùng một kênh thu KienlongBank tại trang Kết nối ngân hàng, rồi xác nhận webhook.' },
  WEBHOOK_UNCONFIRMED: { label: 'Webhook chưa được xác nhận', badge: 'Chờ xác nhận', tone: 'warning', description: 'Owner xác nhận URL webhook với payOS trước khi bật nhận đơn mới.' },
  UNAVAILABLE: { label: 'Thanh toán tạm thời không sẵn sàng', badge: 'Không sẵn sàng', tone: 'danger', description: 'Kiểm tra trạng thái máy chủ và mạng. Không chuyển sang QR tài khoản cũ.' },
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

export function getPaymentStatusDescriptor(state?: string): StatusDescriptor {
  const normalized = (state || '').toUpperCase();
  return PAYMENT_STATUS_MAP[normalized] || { label: state || 'Chưa có trạng thái', badge: state || 'Chưa xác định', tone: 'neutral' };
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
