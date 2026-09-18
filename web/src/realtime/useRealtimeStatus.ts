import { useRealtimeContext } from './RealtimeProvider';
import type { RealtimeStatus } from './realtime.types';

export interface RealtimeStatusInfo {
  status: RealtimeStatus;
  label: string;
  tooltip: string;
  isOnline: boolean;
  lastEventAt: Date | null;
}

export function useRealtimeStatus(): RealtimeStatusInfo {
  const { status, lastEventAt } = useRealtimeContext();

  switch (status) {
    case 'CONNECTED':
      return {
        status,
        label: 'Đang cập nhật trực tiếp',
        tooltip: 'Giao dịch mới sẽ tự xuất hiện mà không cần tải lại trang.',
        isOnline: true,
        lastEventAt,
      };
    case 'CONNECTING':
      return {
        status,
        label: 'Đang kết nối',
        tooltip: 'Đang thiết lập kênh cập nhật trực tiếp.',
        isOnline: false,
        lastEventAt,
      };
    case 'STALE':
      return {
        status,
        label: 'Tín hiệu gián đoạn',
        tooltip: 'Không nhận được nhịp tim từ máy chủ. Đang tự phục hồi kết nối.',
        isOnline: false,
        lastEventAt,
      };
    case 'RECONNECTING':
      return {
        status,
        label: 'Đang kết nối lại',
        tooltip: 'Tạm thời gián đoạn kết nối trực tiếp. Hệ thống đang tự động thử lại.',
        isOnline: false,
        lastEventAt,
      };
    case 'DISCONNECTED':
    default:
      return {
        status,
        label: 'Mất kết nối trực tiếp',
        tooltip: 'Không thể kết nối đến máy chủ cập nhật trực tiếp.',
        isOnline: false,
        lastEventAt,
      };
  }
}
