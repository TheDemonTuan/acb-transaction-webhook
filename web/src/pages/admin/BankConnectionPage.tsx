import React, { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { RefreshCw } from 'lucide-react';
import { confirmPaymentWebhook, fetchPaymentConfig, fetchPaymentOrders, fetchPaymentReviews, fetchStatus } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { getPaymentStatusDescriptor } from '../../content/status-copy';
import { PaymentQRSettingsSection } from '../../features/bank-connection/PaymentQRSettingsSection';
import type { PaymentOrderStatus } from '../../features/payment-qr/payment-orders';
import { formatVndCurrency } from '../../shared/formatters/money';
import { formatDateTimeVN } from '../../shared/formatters/datetime';
import { PaginationControls, useCursorPagination } from '../../shared/ui/PaginationControls';

export const BankConnectionPage: React.FC = () => {
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const [notice, setNotice] = useState<{ error: boolean; text: string } | null>(null);
  const [orderStatus, setOrderStatus] = useState<PaymentOrderStatus>('CREATING');
  const ordersPagination = useCursorPagination(20);
  const reviewsPagination = useCursorPagination(20);
  const status = useQuery({ queryKey: queryKeys.status, queryFn: fetchStatus });
  const config = useQuery({ queryKey: queryKeys.paymentConfig, queryFn: fetchPaymentConfig });
  const orderParams = { status: orderStatus, limit: ordersPagination.pageSize, cursor: ordersPagination.cursor };
  const reviewParams = { limit: reviewsPagination.pageSize, cursor: reviewsPagination.cursor };
  const orders = useQuery({ queryKey: queryKeys.payments(orderParams), queryFn: () => fetchPaymentOrders(orderParams) });
  const reviews = useQuery({ queryKey: queryKeys.paymentReviews(reviewParams), queryFn: () => fetchPaymentReviews(reviewParams), retry: false });
  const provider = status.data?.payments;
  const desc = getPaymentStatusDescriptor(provider?.status);
  const webhookUrl = config.data ? new URL('/api/integrations/payos/webhook', config.data.staticUrl).href : null;
  const refresh = () => { void status.refetch(); void config.refetch(); void orders.refetch(); void reviews.refetch(); };
  const confirm = async () => {
    setConfirming(true);
    setNotice(null);
    try {
      await confirmPaymentWebhook();
      setNotice({ error: false, text: 'payOS đã xác nhận URL webhook. Cần cập nhật PAYOS_WEBHOOK_CONFIRMED trong cấu hình triển khai để mở nhận đơn; giao dịch mẫu không được ghi tiền.' });
      await queryClient.invalidateQueries({ queryKey: queryKeys.status });
      await queryClient.invalidateQueries({ queryKey: queryKeys.paymentConfig });
    } catch (error) {
      setNotice({ error: true, text: error instanceof Error ? error.message : 'Không thể xác nhận webhook.' });
    } finally { setConfirming(false); }
  };

  return <div className="space-y-6">
    <div className="flex items-center justify-between gap-4">
      <div><h2 className="text-2xl font-bold text-stone-900">Kết nối payOS / KienlongBank</h2><p className="text-sm text-stone-600">Cấu hình chỉ đọc, webhook và đối soát đơn thanh toán.</p></div>
      <button type="button" onClick={refresh} className="flex items-center gap-2 rounded-xl border bg-white px-3 py-2 text-xs"><RefreshCw className="h-4 w-4" />Làm mới</button>
    </div>
    {(status.isError || config.isError) && <p role="alert" className="text-rose-700">Không thể tải cấu hình hoặc trạng thái thanh toán. Không dùng QR ngân hàng thay thế.</p>}
    <section className="space-y-4 rounded-2xl border border-stone-200 bg-white p-6">
      <h3 className="font-bold">{provider ? desc.label : 'Đang tải trạng thái'}</h3>
      <p className="text-sm text-stone-600">{desc.description}</p>
      <dl className="space-y-2 text-sm">
        <div><dt className="inline font-semibold">Bộ khóa payOS: </dt><dd className="inline">{provider ? provider.configured ? 'Đã cấu hình đủ' : 'Chưa cấu hình đủ' : 'Đang tải'}</dd></div>
        <div><dt className="inline font-semibold">Webhook được bật trong triển khai: </dt><dd className="inline">{provider ? provider.webhookConfirmed ? 'Có' : 'Chưa' : 'Đang tải'}</dd></div>
        <div><dt className="inline font-semibold">Webhook gần nhất: </dt><dd className="inline">{provider?.lastWebhookAt ? formatDateTimeVN(provider.lastWebhookAt) : 'Chưa ghi nhận'}</dd></div>
        <div><dt className="inline font-semibold">Đối soát gần nhất: </dt><dd className="inline">{provider?.lastReconciledAt ? formatDateTimeVN(provider.lastReconciledAt) : 'Chưa ghi nhận'}</dd></div>
        <div><dt className="inline font-semibold">Đơn chờ / mục cần kiểm tra: </dt><dd className="inline">{provider ? `${provider.pendingOrders} / ${provider.reviewCount}` : 'Đang tải'}</dd></div>
        <div><dt className="font-semibold">URL khách thanh toán</dt><dd className="break-all">{config.data ? <a href={config.data.staticUrl} target="_blank" rel="noreferrer">{config.data.staticUrl}</a> : 'Đang tải'}</dd></div>
        <div><dt className="font-semibold">URL webhook cố định</dt><dd className="break-all font-mono text-xs">{webhookUrl || 'Đang tải'}</dd></div>
      </dl>
      <p className="text-xs text-stone-600">Ba khóa phải thuộc cùng một kênh thu KienlongBank dành riêng cho ứng dụng. Khóa được quản lý trên máy chủ; trang này không hiển thị hay nhập khóa, mật khẩu hoặc OTP. Không có giao dịch mới không có nghĩa webhook bị lỗi.</p>
      <button type="button" disabled={confirming || !provider?.configured} onClick={() => void confirm()} className="rounded-xl bg-stone-900 px-4 py-2 text-sm text-white disabled:opacity-50">{confirming ? 'Đang xác nhận...' : 'Xác nhận webhook (Owner)'}</button>
      {notice && <p role={notice.error ? 'alert' : 'status'} className={notice.error ? 'text-rose-700 text-sm' : 'text-emerald-700 text-sm'}>{notice.text}</p>}
      <a href="#payment-reviews" className="block text-sm underline">Xem callback cần kiểm tra</a>
    </section>
    <section className="space-y-4 rounded-2xl border border-stone-200 bg-white p-6">
      <h3 className="font-bold">Đơn cần đối soát</h3>
      <p className="text-xs text-stone-600">Các đơn đang tạo, xử lý hoặc thiếu tiền được đối soát từ máy chủ. Mã lỗi như PAYMENT_DETAILS_PENDING, CREATE_OUTCOME_UNKNOWN hoặc lỗi phục hồi QR cần được kiểm tra, kể cả khi không có callback trong inbox.</p>
      <label className="flex gap-3 items-center text-sm">Trạng thái đơn<select value={orderStatus} onChange={(event) => { setOrderStatus(event.target.value as PaymentOrderStatus); ordersPagination.reset(); }} className="rounded-lg border px-3 py-2">
        {(['CREATING', 'PROCESSING', 'UNDERPAID', 'PENDING', 'FAILED', 'CANCELLED', 'EXPIRED'] as const).map(value => <option key={value} value={value}>{value}</option>)}
      </select></label>
      {orders.isLoading ? <p>Đang tải đơn...</p> : orders.isError ? <p role="alert" className="text-rose-700">Không thể tải đơn: {orders.error.message}</p> : !orders.data?.items.length ? <p className="text-sm text-stone-600">Không có đơn trong trạng thái này.</p> : <ul className="divide-y divide-stone-100">{orders.data.items.map(order => <li key={order.id} className="py-3 text-sm space-y-1">
        <div className="flex flex-wrap gap-3"><strong className="font-mono">{order.orderCode}</strong><span>{formatVndCurrency(order.amountVnd)}</span><span>{order.status}</span></div>
        <p className="text-xs text-stone-600">Tạo lúc {formatDateTimeVN(order.createdAt)} · {order.origin}</p>
        {order.errorCode && <p className="font-mono text-xs text-amber-800">{order.errorCode}</p>}
      </li>)}</ul>}
      <PaginationControls pageNumber={ordersPagination.pageNumber} itemCount={orders.data?.items.length ?? 0} pageSize={ordersPagination.pageSize} hasNext={Boolean(orders.data?.nextCursor)} hasPrev={ordersPagination.hasPrev} isLoading={orders.isFetching} onNext={() => ordersPagination.handleNext(orders.data?.nextCursor)} onPrev={ordersPagination.handlePrev} onFirst={ordersPagination.handleFirst} onPageSizeChange={ordersPagination.setPageSize} />
    </section>
    <section id="payment-reviews" className="space-y-4 rounded-2xl border border-stone-200 bg-white p-6">
      <h3 className="font-bold">Callback cần kiểm tra (Owner / Operator)</h3>
      {reviews.isLoading ? <p>Đang tải callback...</p> : reviews.isError ? <p role="alert" className="text-rose-700">Không thể tải callback hoặc không đủ quyền: {reviews.error.message}</p> : !reviews.data?.items.length ? <p className="text-sm text-stone-600">Không có callback cần kiểm tra.</p> : <ul className="divide-y divide-stone-100">{reviews.data.items.map(review => <li key={review.payloadHash} className="py-3 text-sm space-y-1">
        <p className="font-mono">Đơn {review.orderCode || 'Không xác định'} · {review.reason}</p>
        {review.reference && <p className="text-xs">Tham chiếu: {review.reference}</p>}
        <p className="text-xs text-stone-600">{formatDateTimeVN(review.receivedAt)}{review.processedAt ? ' · Đã xử lý' : ' · Chờ kiểm tra'}</p>
      </li>)}</ul>}
      <PaginationControls pageNumber={reviewsPagination.pageNumber} itemCount={reviews.data?.items.length ?? 0} pageSize={reviewsPagination.pageSize} hasNext={Boolean(reviews.data?.nextCursor)} hasPrev={reviewsPagination.hasPrev} isLoading={reviews.isFetching} onNext={() => reviewsPagination.handleNext(reviews.data?.nextCursor)} onPrev={reviewsPagination.handlePrev} onFirst={reviewsPagination.handleFirst} onPageSizeChange={reviewsPagination.setPageSize} />
    </section>
    <PaymentQRSettingsSection />
  </div>;
};
