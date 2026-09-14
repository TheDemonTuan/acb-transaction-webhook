import { describe, expect, it } from 'vitest';
import {
  getAcbStatusDescriptor,
  getDeliveryStatus,
  getNotificationChannelStatus,
  getServiceStatus,
  getWebhookStatus,
} from '../src/content/status-copy';

describe('status-copy', () => {
  it('maps ACB states to user-friendly Vietnamese labels without exposing raw technical enum', () => {
    expect(getAcbStatusDescriptor('UNCONFIGURED').label).toBe('Chưa kết nối ngân hàng');
    expect(getAcbStatusDescriptor('AUTH_REQUIRED').label).toBe('Cần đăng nhập lại ACB');
    expect(getAcbStatusDescriptor('MONITORING').label).toBe('Đang cập nhật giao dịch');
    expect(getAcbStatusDescriptor('PAUSED').label).toBe('Đang tạm dừng');
  });

  it('maps Service statuses', () => {
    expect(getServiceStatus('HEALTHY').label).toBe('Hệ thống hoạt động bình thường');
    expect(getServiceStatus('HEALTHY').tone).toBe('success');
  });

  it('maps Webhook & Delivery statuses', () => {
    expect(getWebhookStatus('ACTIVE').label).toBe('Đang hoạt động');
    expect(getDeliveryStatus('SUCCESS').label).toBe('Thành công');
    expect(getDeliveryStatus('DELIVERED').label).toBe('Thành công');
    expect(getDeliveryStatus('IN_FLIGHT').label).toBe('Đang xử lý');
    expect(getDeliveryStatus('DEAD_LETTER').label).toBe('Không gửi được');
    expect(getNotificationChannelStatus('ACTIVE').label).toBe('Đang hoạt động');
    expect(getNotificationChannelStatus('DISABLED').label).toBe('Đang tắt');
  });
});
