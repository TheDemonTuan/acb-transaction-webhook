import React from 'react';
import { useQuery } from '@tanstack/react-query';
import {
  Server,
  Database,
  RefreshCw,
  CheckCircle2,
  Activity,
} from 'lucide-react';
import { fetchSePayReviews, fetchStatus } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';


const recentSePayReviewParams = { limit: 20 };
export const SystemPage: React.FC = () => {
  const { data: status, isLoading, isError, refetch } = useQuery({
    queryKey: queryKeys.status,
    queryFn: fetchStatus,
  });
  const canReviewSePay = status?.userRole === 'OWNER' || status?.userRole === 'OPERATOR';
  const reviews = useQuery({
    queryKey: queryKeys.sepayReviews(recentSePayReviewParams),
    queryFn: () => fetchSePayReviews(recentSePayReviewParams),
    enabled: canReviewSePay,
    retry: false,
    gcTime: 0,
  });

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h2 className="text-2xl font-bold tracking-tight text-stone-900">
            Chẩn đoán hệ thống
          </h2>
          <p className="text-sm text-stone-500 mt-0.5">
            Thông số kỹ thuật, tình trạng tài nguyên lưu trữ và nhật ký tiến trình
          </p>
        </div>
        <button
          type="button"
          onClick={() => {
            void refetch();
            if (canReviewSePay) void reviews.refetch();
          }}
          disabled={isLoading}
          className="inline-flex items-center self-start sm:self-auto gap-2 px-3 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer"
        >
          <RefreshCw className={`w-3.5 h-3.5 ${isLoading ? 'animate-spin' : ''}`} />
          <span>Làm mới</span>
        </button>
      </div>
      {isError && <p role="alert" className="text-rose-700">Không thể tải trạng thái hệ thống.</p>}

      {/* Health Overview */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-stone-500 uppercase tracking-wider">
              Dịch vụ lõi
            </span>
            <div className="p-2 rounded-xl bg-emerald-50 text-emerald-600">
              <Server className="w-4 h-4" />
            </div>
          </div>
          <div className="mt-3">
            <div className="flex items-center gap-2">
              <span className="text-xl font-bold text-stone-900">
                {status?.service || 'Chưa có dữ liệu'}
              </span>
              {status?.service === 'HEALTHY' && <CheckCircle2 className="w-4 h-4 text-emerald-600" />}
            </div>
            <span className="text-xs text-stone-500 block mt-1">
              Phiên bản hệ thống: {status?.version || 'Chưa có dữ liệu'}
            </span>
          </div>
        </div>

        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-stone-500 uppercase tracking-wider">
              Thời gian chạy (Uptime)
            </span>
            <div className="p-2 rounded-xl bg-blue-50 text-blue-600">
              <Activity className="w-4 h-4" />
            </div>
          </div>
          <div className="mt-3">
            <span className="text-xl font-bold text-stone-900">
              {Math.floor((status?.uptimeSeconds || 0) / 60)} phút
            </span>
            <span className="text-xs text-stone-500 block mt-1">
              ({status?.uptimeSeconds || 0} giây)
            </span>
          </div>
        </div>

        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-stone-500 uppercase tracking-wider">
              Cơ sở dữ liệu
            </span>
            <div className="p-2 rounded-xl bg-emerald-50 text-emerald-600">
              <Database className="w-4 h-4" />
            </div>
          </div>
          <div className="mt-3">
            <span className="text-xl font-bold text-stone-900">
              {status?.storage?.status || 'Chưa có dữ liệu'}
            </span>
            <span className="text-xs text-stone-500 block mt-1">
              Chế độ an toàn cao &middot; Giao dịch chuẩn ACID
            </span>
          </div>
        </div>
      </div>

      <section className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs space-y-4" aria-labelledby="sepay-system-heading">
        <h3 id="sepay-system-heading" className="font-bold text-stone-900">SePay Store · Nhận thông báo</h3>
        <dl className="grid grid-cols-1 sm:grid-cols-3 gap-3 text-sm">
          <div><dt className="text-stone-500">Chế độ</dt><dd className="font-semibold text-stone-900">{status?.sepay?.mode ?? 'Đang tải'}</dd></div>
          <div><dt className="text-stone-500">Lần nhận hợp lệ cuối</dt><dd className="font-semibold text-stone-900 break-words">{status?.sepay?.lastMessageAt || 'Chưa ghi nhận'}</dd></div>
          <div><dt className="text-stone-500">Mục cần đối chiếu</dt><dd className="font-semibold text-stone-900">{status?.sepay?.reviewCount ?? 'Đang tải'}</dd></div>
        </dl>
        <p className="text-xs text-stone-500">Chế độ active chỉ cho biết đã bật nhận thông báo, không bảo đảm kết nối ngân hàng. Đối chiếu thông báo với Store/Telegram; không tự ghi lại giao dịch.</p>
        {canReviewSePay && (
          <div className="space-y-3">
            <h4 className="text-sm font-semibold text-stone-900">20 mục đối chiếu gần nhất</h4>
            {reviews.isPending && <p className="text-sm text-stone-500">Đang tải mục đối chiếu…</p>}
            {reviews.isError && <p role="alert" className="text-sm text-rose-700">Không thể tải mục đối chiếu SePay.</p>}
            {reviews.data && reviews.data.items.length === 0 && <p className="text-sm text-stone-500">Không có mục cần đối chiếu.</p>}
            {reviews.data && reviews.data.items.length > 0 && (
              <ul className="divide-y divide-stone-100">
                {reviews.data.items.map((review, index) => (
                  <li key={`${review.storeKey}:${review.messageId}:${review.receivedAt}:${index}`} className="py-3 text-sm break-words">
                    <p className="font-semibold text-stone-900">{review.reason}</p>
                    <p className="text-stone-600">{review.storeKey} · Tin {review.messageId}</p>
                    <time className="text-xs text-stone-500" dateTime={review.receivedAt}>{review.receivedAt}</time>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </section>

      {/* Technical Diagnostics Details */}
      <div className="bg-white p-6 rounded-2xl border border-stone-200 shadow-2xs space-y-4">
        <h3 className="font-bold text-stone-900 text-base">Thông số vận hành chi tiết</h3>
        <p className="text-sm text-stone-600">
          Trạng thái payOS/KienlongBank dựa trên cấu hình và xử lý giao dịch, không phụ thuộc nhịp polling ngân hàng.
        </p>

        <div className="divide-y divide-stone-100 text-xs font-mono">
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Kênh thanh toán:</span>
            <span className="font-bold text-stone-900">
              {status?.payments ? `${status.payments.provider} / ${status.payments.bank}` : 'Đang tải'}
            </span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Trạng thái nhận đơn:</span>
            <span className="font-bold text-stone-900">{status?.payments?.status ?? 'Đang tải'}</span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Đơn đang chờ xử lý:</span>
            <span className="font-bold text-stone-900">{status?.payments?.pendingOrders ?? 'Đang tải'}</span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Mục cần kiểm tra:</span>
            <span className="font-bold text-stone-900">{status?.payments?.reviewCount ?? 'Đang tải'}</span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Webhook / đối soát gần nhất:</span>
            <span className="font-bold text-stone-900">{status?.payments?.lastWebhookAt || 'Chưa ghi nhận'} / {status?.payments?.lastReconciledAt || 'Chưa ghi nhận'}</span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Thông báo đang xếp hàng gửi:</span>
            <span className="font-bold text-stone-900">
              {status?.notifications?.total.pending ?? status?.webhooks?.pending ?? 0}
            </span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Thông báo gửi không thành công:</span>
            <span className="font-bold text-stone-900">
              {status?.notifications?.total.deadLetter ?? status?.webhooks?.deadLetter ?? 0}
            </span>
          </div>
        </div>
      </div>
    </div>
  );
};
