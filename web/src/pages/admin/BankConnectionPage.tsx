import React, { useEffect, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { RefreshCw } from 'lucide-react';
import { confirmPaymentWebhook, fetchPaymentConfig, fetchPaymentOrders, fetchPaymentProviderConfig, fetchPaymentReviews, fetchStatus, savePaymentProviderConfig } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { getPaymentStatusDescriptor } from '../../content/status-copy';
import { PaymentQRSettingsSection } from '../../features/bank-connection/PaymentQRSettingsSection';
import { SePaySettingsSection } from '../../features/bank-connection/SePaySettingsSection';
import type { PaymentOrderStatus, PaymentProviderConfig } from '../../features/payment-qr/payment-orders';
import { formatVndCurrency } from '../../shared/formatters/money';
import { formatDateTimeVN } from '../../shared/formatters/datetime';
import { PaginationControls, useCursorPagination } from '../../shared/ui/PaginationControls';
import { ApiError } from '../../api';

const configurationError = (error: unknown): string => {
  if (error instanceof ApiError) {
    if (error.code === 'PROVIDER_CHANNEL_LOCKED') return 'Không thể đổi kênh thu sau khi đã phát hành đơn. Giữ Client ID hiện tại để bảo vệ các đơn và lịch sử thanh toán.';
    if (error.code === 'PROVIDER_CONFIG_BUSY') return 'Đang có yêu cầu thanh toán hoặc đối soát. Vui lòng đợi hoàn tất rồi lưu lại.';
    if (error.status === 403) return 'Chỉ Owner được lưu bộ khóa hoặc xác nhận webhook. Vui lòng kiểm tra tài khoản đăng nhập.';
  }
  return 'Không thể hoàn tất cấu hình payOS. Kiểm tra kết nối và bộ khóa cùng kênh thu, rồi thử lại. Không có khóa nào được hiển thị trong thông báo lỗi.';
};

const OwnerPaymentProviderForm: React.FC<{
  config: PaymentProviderConfig;
  onSaved: (config: PaymentProviderConfig) => void;
}> = ({ config, onSaved }) => {
  const [clientId, setClientId] = useState(config.clientId ?? '');
  const [enabled, setEnabled] = useState(config.enabled);
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [notice, setNotice] = useState<{ error: boolean; text: string } | null>(null);
  useEffect(() => {
    if (!dirty) {
      setClientId(config.clientId ?? '');
      setEnabled(config.enabled);
    }
  }, [config, dirty]);
  const keysRequired = !config.configured || clientId.trim() !== config.clientId;
  const busy = saving || confirming;

  const save = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (busy) return;
    const form = event.currentTarget;
    setSaving(true);
    setNotice(null);
    try {
      const fields = new FormData(form);
      const saved = await savePaymentProviderConfig({
        clientId: clientId.trim(),
        apiKey: String(fields.get('apiKey') ?? '').trim(),
        checksumKey: String(fields.get('checksumKey') ?? '').trim(),
        enabled,
      });
      // Secrets live only in these uncontrolled password inputs, never browser storage or mutation/query caches.
      form.reset();
      setClientId(saved.clientId ?? '');
      setEnabled(saved.enabled);
      setDirty(false);
      onSaved(saved);
      setNotice({ error: false, text: saved.webhookConfirmed
        ? saved.enabled ? 'Đã lưu cấu hình. Hệ thống được bật nhận đơn mới.' : 'Đã lưu cấu hình. Tạm dừng nhận đơn mới; các đơn đã phát hành vẫn được đối soát.'
        : 'Đã lưu bộ khóa. Bấm xác nhận webhook bên dưới để kiểm tra kết nối thực với payOS trước khi nhận đơn mới.' });
    } catch (error) {
      setNotice({ error: true, text: configurationError(error) });
    } finally { setSaving(false); }
  };
  const confirm = async () => {
    if (busy || dirty || !config.configured) return;
    setConfirming(true);
    setNotice(null);
    try {
      await confirmPaymentWebhook();
      onSaved({ ...config, webhookConfirmed: true });
      setNotice({ error: false, text: config.enabled
        ? 'payOS đã xác nhận webhook. Hệ thống được bật nhận đơn mới; giao dịch mẫu không được ghi tiền.'
        : 'payOS đã xác nhận webhook. Bật nhận đơn mới và lưu cấu hình khi sẵn sàng; giao dịch mẫu không được ghi tiền.' });
    } catch (error) {
      setNotice({ error: true, text: configurationError(error) });
    } finally { setConfirming(false); }
  };

  return <form onSubmit={(event) => void save(event)} onChange={() => { setDirty(true); setNotice(null); }} autoComplete="off" className="space-y-4">
    <h3 className="font-bold">Bộ khóa kênh thu KienlongBank trên payOS</h3>
    {!config.configured && <ol className="list-decimal pl-5 text-sm text-stone-600 space-y-1">
      <li>Mở dashboard payOS, tạo hoặc chọn một kênh thu đã liên kết KienlongBank dành riêng cho ứng dụng này.</li>
      <li>Sao chép Client ID, API Key và Checksum Key của cùng kênh thu vào ba ô bên dưới, rồi lưu.</li>
      <li>Xác nhận webhook bằng nút bên dưới. Chỉ khi xác nhận thành công và bật nhận đơn mới, khách mới tạo được đơn.</li>
    </ol>}
    <fieldset disabled={busy} className="space-y-4 disabled:opacity-60">
      <label className="block text-sm font-semibold">Client ID
        <input name="clientId" type="password" autoComplete="off" spellCheck={false} required value={clientId} onChange={(event) => setClientId(event.target.value)} className="mt-1 block w-full rounded-xl border border-stone-300 px-3 py-2 font-mono" />
      </label>
      <label className="block text-sm font-semibold">API Key
        <span className="ml-2 text-xs font-normal text-stone-500">{config.apiKeyConfigured ? 'Đã lưu ••••••••' : 'Chưa lưu'}</span>
        <input name="apiKey" type="password" autoComplete="new-password" spellCheck={false} required={keysRequired} className="mt-1 block w-full rounded-xl border border-stone-300 px-3 py-2 font-mono" />
      </label>
      <label className="block text-sm font-semibold">Checksum Key
        <span className="ml-2 text-xs font-normal text-stone-500">{config.checksumKeyConfigured ? 'Đã lưu ••••••••' : 'Chưa lưu'}</span>
        <input name="checksumKey" type="password" autoComplete="new-password" spellCheck={false} required={keysRequired} className="mt-1 block w-full rounded-xl border border-stone-300 px-3 py-2 font-mono" />
      </label>
      <p className="text-xs text-stone-600">Khóa bí mật không được đọc lại. Để trống API Key và Checksum Key để giữ khóa đã lưu của cùng Client ID. Đổi khóa sẽ yêu cầu xác nhận webhook lại. Không dùng chung kênh thu này với ứng dụng khác.</p>
      <label className="flex items-center gap-3 text-sm font-semibold"><input type="checkbox" name="enabled" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />Bật nhận đơn mới</label>
      <p className="text-xs text-stone-600">Tắt chỉ dừng tạo đơn mới; webhook và đối soát các đơn đã phát hành vẫn tiếp tục.</p>
      <button type="submit" disabled={!dirty} className="rounded-xl bg-emerald-700 px-4 py-2 text-sm text-white disabled:opacity-50">{saving ? 'Đang lưu...' : 'Lưu cấu hình payOS'}</button>
    </fieldset>
    <button type="button" disabled={busy || dirty || !config.configured} onClick={() => void confirm()} className="rounded-xl bg-stone-900 px-4 py-2 text-sm text-white disabled:opacity-50">{confirming ? 'Đang xác nhận...' : 'Xác nhận webhook'}</button>
    {dirty && <p className="text-xs text-stone-600">Lưu thay đổi trước khi xác nhận webhook.</p>}
    {notice && <p role={notice.error ? 'alert' : 'status'} className={notice.error ? 'text-rose-700 text-sm' : 'text-emerald-700 text-sm'}>{notice.text}</p>}
  </form>;
};

export const BankConnectionPage: React.FC = () => {
  const queryClient = useQueryClient();
  const status = useQuery({ queryKey: queryKeys.status, queryFn: fetchStatus });
  const isOwner = status.data?.userRole === 'OWNER';
  const canReview = isOwner || status.data?.userRole === 'OPERATOR';
  const ownerConfig = useQuery({ queryKey: queryKeys.paymentProviderConfig, queryFn: fetchPaymentProviderConfig, enabled: isOwner, retry: false, gcTime: 0 });
  const [orderStatus, setOrderStatus] = useState<PaymentOrderStatus>('CREATING');
  const ordersPagination = useCursorPagination(20);
  const reviewsPagination = useCursorPagination(20);
  const config = useQuery({ queryKey: queryKeys.paymentConfig, queryFn: fetchPaymentConfig });
  const orderParams = { status: orderStatus, limit: ordersPagination.pageSize, cursor: ordersPagination.cursor };
  const reviewParams = { limit: reviewsPagination.pageSize, cursor: reviewsPagination.cursor };
  const orders = useQuery({ queryKey: queryKeys.payments(orderParams), queryFn: () => fetchPaymentOrders(orderParams) });
  const reviews = useQuery({ queryKey: queryKeys.paymentReviews(reviewParams), queryFn: () => fetchPaymentReviews(reviewParams), enabled: canReview, retry: false });
  const provider = status.data?.payments;
  const desc = getPaymentStatusDescriptor(provider?.status);
  const webhookUrl = ownerConfig.data?.webhookUrl ?? (config.data ? new URL('/api/integrations/payos/webhook', config.data.staticUrl).href : null);
  const refresh = () => {
    void status.refetch(); void config.refetch(); void orders.refetch();
    if (isOwner) void ownerConfig.refetch();
    if (canReview) void reviews.refetch();
    if (isOwner) void queryClient.invalidateQueries({ queryKey: queryKeys.sepayAdminConfig });
    if (canReview) void queryClient.invalidateQueries({ queryKey: ['sepay-reviews'] });
  };
  const providerSaved = (saved: PaymentProviderConfig) => {
    queryClient.setQueryData(queryKeys.paymentProviderConfig, saved);
    void queryClient.invalidateQueries({ queryKey: queryKeys.status });
    void queryClient.invalidateQueries({ queryKey: queryKeys.paymentConfig });
  };

  return <div className="space-y-6">
    <div className="flex items-center justify-between gap-4">
      <div><h2 className="text-2xl font-bold text-stone-900">Kết nối ngân hàng</h2><p className="text-sm text-stone-600">SePay Store cho QR cửa hàng · payOS cho đơn theo số tiền.</p></div>
      <button type="button" onClick={refresh} className="flex items-center gap-2 rounded-xl border bg-white px-3 py-2 text-xs"><RefreshCw className="h-4 w-4" />Làm mới</button>
    </div>
    {(status.isError || config.isError) && <p role="alert" className="text-rose-700">Không thể tải cấu hình hoặc trạng thái thanh toán. Không dùng QR ngân hàng thay thế.</p>}
    <SePaySettingsSection isOwner={isOwner} canReview={canReview} status={status.data?.sepay} />
    <section className="space-y-4 rounded-2xl border border-stone-200 bg-white p-6">
      <h3 className="font-bold">{provider ? desc.label : 'Đang tải trạng thái'}</h3>
      <p className="text-sm text-stone-600">{desc.description}</p>
      <dl className="space-y-2 text-sm">
        <div><dt className="inline font-semibold">Bộ khóa payOS: </dt><dd className="inline">{provider ? provider.configured ? 'Đã cấu hình đủ' : 'Chưa cấu hình đủ' : 'Đang tải'}</dd></div>
        <div><dt className="inline font-semibold">Webhook đã xác nhận với payOS: </dt><dd className="inline">{provider ? provider.webhookConfirmed ? 'Có' : 'Chưa' : 'Đang tải'}</dd></div>
        <div><dt className="inline font-semibold">Webhook gần nhất: </dt><dd className="inline">{provider?.lastWebhookAt ? formatDateTimeVN(provider.lastWebhookAt) : 'Chưa ghi nhận'}</dd></div>
        <div><dt className="inline font-semibold">Đối soát gần nhất: </dt><dd className="inline">{provider?.lastReconciledAt ? formatDateTimeVN(provider.lastReconciledAt) : 'Chưa ghi nhận'}</dd></div>
        <div><dt className="inline font-semibold">Đơn chờ / mục cần kiểm tra: </dt><dd className="inline">{provider ? `${provider.pendingOrders} / ${provider.reviewCount}` : 'Đang tải'}</dd></div>
        <div><dt className="font-semibold">URL khách thanh toán</dt><dd className="break-all">{config.data ? <a href={config.data.staticUrl} target="_blank" rel="noreferrer">{config.data.staticUrl}</a> : 'Đang tải'}</dd></div>
        <div><dt className="font-semibold">URL webhook cố định</dt><dd className="break-all font-mono text-xs">{webhookUrl || 'Đang tải'}</dd></div>
      </dl>
      <p className="text-xs text-stone-600">Ba khóa phải thuộc cùng một kênh thu KienlongBank dành riêng cho ứng dụng. Không có giao dịch mới không có nghĩa webhook bị lỗi.</p>
      {isOwner ? ownerConfig.isLoading ? <p>Đang tải cấu hình Owner...</p> : ownerConfig.isError ? <p role="alert" className="text-sm text-rose-700">Không thể tải cấu hình Owner. Hãy làm mới để thử lại.</p> : ownerConfig.data && <OwnerPaymentProviderForm config={ownerConfig.data} onSaved={providerSaved} /> : <p className="text-sm text-stone-600">Chỉ Owner được nhập bộ khóa, bật nhận đơn và xác nhận webhook. Liên hệ Owner để cấu hình kênh thu; trang này chỉ hiển thị trạng thái.</p>}
      {canReview && <a href="#payment-reviews" className="block text-sm underline">Xem callback cần kiểm tra</a>}
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
    {canReview && <section id="payment-reviews" className="space-y-4 rounded-2xl border border-stone-200 bg-white p-6">
      <h3 className="font-bold">Callback cần kiểm tra (Owner / Operator)</h3>
      {reviews.isLoading ? <p>Đang tải callback...</p> : reviews.isError ? <p role="alert" className="text-rose-700">Không thể tải callback hoặc không đủ quyền: {reviews.error.message}</p> : !reviews.data?.items.length ? <p className="text-sm text-stone-600">Không có callback cần kiểm tra.</p> : <ul className="divide-y divide-stone-100">{reviews.data.items.map(review => <li key={review.payloadHash} className="py-3 text-sm space-y-1">
        <p className="font-mono">Đơn {review.orderCode || 'Không xác định'} · {review.reason}</p>
        {review.reference && <p className="text-xs">Tham chiếu: {review.reference}</p>}
        <p className="text-xs text-stone-600">{formatDateTimeVN(review.receivedAt)}{review.processedAt ? ' · Đã xử lý' : ' · Chờ kiểm tra'}</p>
      </li>)}</ul>}
      <PaginationControls pageNumber={reviewsPagination.pageNumber} itemCount={reviews.data?.items.length ?? 0} pageSize={reviewsPagination.pageSize} hasNext={Boolean(reviews.data?.nextCursor)} hasPrev={reviewsPagination.hasPrev} isLoading={reviews.isFetching} onNext={() => reviewsPagination.handleNext(reviews.data?.nextCursor)} onPrev={reviewsPagination.handlePrev} onFirst={reviewsPagination.handleFirst} onPageSizeChange={reviewsPagination.setPageSize} />
    </section>}
    <PaymentQRSettingsSection />
  </div>;
};
