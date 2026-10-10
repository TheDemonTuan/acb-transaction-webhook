import React, { useEffect, useRef, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import QRCode from 'qrcode';
import { ApiError } from '../../api';
import { isPublicViewerHost } from '../../app/runtime-mode';
import { createPublicPaymentOrder, fetchPaymentConfig, fetchPublicPaymentOrder } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { formatVndCurrency } from '../../shared/formatters/money';
import { formatDateTimeVN } from '../../shared/formatters/datetime';
import { useRealtimeContext, useRealtimeSubscription } from '../../realtime/RealtimeProvider';
import type { BankTransactionCreditData } from '../../realtime/realtime.types';
import { creditMatchesPaymentOrder, isTerminalPaymentOrder, parsePaymentAmountVnd, type PaymentOrderStatus } from '../../features/payment-qr/payment-orders';
import { canonicalPaymentRedirect, clearGuestPaymentIntent, loadGuestPaymentIntent, saveGuestPaymentIntent, type GuestPaymentIntent } from '../../features/payment-qr/guest-payment-intent';

const readinessMessages = {
  READY: '',
  DISABLED: 'Cửa hàng đang tạm ngừng tạo đơn thanh toán mới.',
  UNCONFIGURED: 'Cửa hàng chưa hoàn tất cấu hình payOS/KienlongBank.',
  WEBHOOK_UNCONFIRMED: 'Cửa hàng đang xác nhận kết nối thanh toán. Vui lòng thử lại sau.',
  UNAVAILABLE: 'Dịch vụ thanh toán đang chưa sẵn sàng. Vui lòng thử lại sau.',
};
const statusLabels: Record<PaymentOrderStatus, string> = {
  CREATING: 'Đang tạo đơn thanh toán', PENDING: 'Đang chờ thanh toán', PROCESSING: 'Đang chờ xác nhận',
  UNDERPAID: 'Đang đối soát số tiền', PAID: 'Thanh toán thành công', CANCELLED: 'Đơn đã hủy',
  EXPIRED: 'Đơn đã hết hạn', FAILED: 'Không thể tạo thanh toán',
};
const buttonClass = 'w-full rounded-xl bg-stone-900 px-4 py-3 text-white font-semibold disabled:opacity-50 disabled:cursor-not-allowed';
const secondaryClass = 'rounded-xl border border-stone-300 px-4 py-2 text-sm font-medium hover:bg-stone-100';

export const PayPage: React.FC = () => {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const realtime = useRealtimeContext();
  const [intent, setIntent] = useState<GuestPaymentIntent | null>(() => {
    try { return loadGuestPaymentIntent(); } catch { return null; }
  });
  const [amount, setAmount] = useState(() => intent ? String(intent.amountVnd) : '');
  const [submitting, setSubmitting] = useState(false);
  const submittingRef = useRef(false);
  const [error, setError] = useState('');
  const [copyMessage, setCopyMessage] = useState('');
  const [qrImage, setQrImage] = useState('');
  const [qrError, setQrError] = useState(false);
  const [now, setNow] = useState(Date.now());
  const config = useQuery({ queryKey: queryKeys.paymentConfig, queryFn: fetchPaymentConfig, retry: false });
  const snapshot = useQuery({
    queryKey: queryKeys.publicPaymentOrder(id ?? ''),
    queryFn: () => fetchPublicPaymentOrder(id!),
    enabled: Boolean(id),
    staleTime: 0,
    refetchOnMount: 'always',
    retry: false,
    refetchInterval: (query) => query.state.data && isTerminalPaymentOrder(query.state.data.status) ? false : 5_000,
    refetchIntervalInBackground: true,
  });
  const order = snapshot.data;
  const expiredByClock = Boolean(order && Date.parse(order.expiresAt) <= now);
  const terminal = Boolean(order && isTerminalPaymentOrder(order.status));
  const canPay = Boolean(order && !terminal && !expiredByClock && order.qrCode);
  const ready = config.data?.ready === true;
  const parsedAmount = parsePaymentAmountVnd(amount, config.data?.maxAmountVnd);

  useEffect(() => {
    // Capability URLs must not become a referrer, including on canonical redirects.
    const existing = document.querySelector<HTMLMetaElement>('meta[name="referrer"]');
    const meta = existing ?? document.createElement('meta');
    const previous = existing?.content;
    meta.name = 'referrer';
    meta.content = 'no-referrer';
    if (!existing) document.head.appendChild(meta);
    return () => { if (existing) meta.content = previous ?? ''; else meta.remove(); };
  }, []);

  useEffect(() => {
    if (!config.data) return;
    const target = canonicalPaymentRedirect(config.data.staticUrl, window.location.href, isPublicViewerHost());
    if (target) window.location.replace(target);
  }, [config.data]);

  useEffect(() => {
    if (!id) return;
    const refresh = () => { void snapshot.refetch(); };
    window.addEventListener('pageshow', refresh);
    window.addEventListener('focus', refresh);
    window.addEventListener('online', refresh);
    return () => {
      window.removeEventListener('pageshow', refresh);
      window.removeEventListener('focus', refresh);
      window.removeEventListener('online', refresh);
    };
  }, [id, snapshot.refetch]);

  useEffect(() => {
    if (id && realtime.status === 'CONNECTED') void snapshot.refetch();
  }, [id, realtime.status, realtime.reconnectCount, snapshot.refetch]);

  useRealtimeSubscription<BankTransactionCreditData>('bank.transaction.credit', ({ data }) => {
    if (order && creditMatchesPaymentOrder(order, data)) void snapshot.refetch();
  });

  useEffect(() => {
    if (!order || terminal) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, [order?.id, terminal]);

  useEffect(() => {
    let active = true;
    setQrImage('');
    setQrError(false);
    if (canPay && order?.qrCode) {
      QRCode.toDataURL(order.qrCode, { width: 480, margin: 2, errorCorrectionLevel: 'M' }).then(
        (image) => { if (active) setQrImage(image); },
        () => { if (active) setQrError(true); },
      );
    }
    return () => { active = false; };
  }, [canPay, order?.qrCode]);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (submittingRef.current) return;
    if (intent?.orderId) { navigate(`/pay/${intent.orderId}`); return; }
    if (!ready || parsedAmount === null) {
      setError('Nhập số tiền VND nguyên, từ 1 đến giới hạn cửa hàng. Ví dụ: 50000 = 50.000đ.');
      return;
    }
    submittingRef.current = true;
    setSubmitting(true);
    setError('');
    try {
      // Read storage again: an uncertain earlier POST must reuse its original key.
      const saved = loadGuestPaymentIntent();
      const next = saved ?? intent ?? { idempotencyKey: crypto.randomUUID(), amountVnd: parsedAmount };
      if (next.amountVnd !== parsedAmount) throw new Error('Payment intent amount changed');
      saveGuestPaymentIntent(next);
      setIntent(next);
      const created = await createPublicPaymentOrder(next.amountVnd, 'STATIC_URL', next.idempotencyKey);
      const bound = { ...next, orderId: created.id };
      setIntent(bound);
      saveGuestPaymentIntent(bound);
      queryClient.setQueryData(queryKeys.publicPaymentOrder(created.id), created);
      navigate(`/pay/${created.id}`);
    } catch (cause) {
      setError(cause instanceof ApiError && cause.code === 'INVALID_AMOUNT'
        ? 'Số tiền không hợp lệ. Vui lòng kiểm tra số tiền VND.'
        : cause instanceof ApiError && cause.status === 503
          ? 'Thanh toán đang tạm thời chưa sẵn sàng. Thử lại vẫn dùng cùng đơn, không tạo trùng.'
          : 'Chưa thể xác nhận kết quả tạo đơn hoặc lưu phiên thanh toán. Giữ trang này và thử lại; không tạo đơn trùng.');
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  };

  const copy = async (value: string) => {
    try { await navigator.clipboard.writeText(value); setCopyMessage('Đã sao chép.'); }
    catch { setCopyMessage('Không thể sao chép tự động. Bạn có thể chọn và sao chép thông tin bên dưới.'); }
  };
  const startNew = () => {
    try {
      // Do not discard a separate payment intent held in this tab.
      if (!intent || intent.orderId === id) { clearGuestPaymentIntent(); setIntent(null); setAmount(''); }
      setError('');
      navigate('/pay');
    } catch { setError('Không thể lưu phiên thanh toán. Vui lòng cho phép bộ nhớ phiên trước khi tạo đơn mới.'); }
  };
  const openCheckout = (event: React.MouseEvent<HTMLAnchorElement>) => {
    try {
      if (intent && intent.orderId === id) saveGuestPaymentIntent(intent);
      else if (order) sessionStorage.setItem('guest_payment_checkout_order_v1', order.id);
    } catch {
      event.preventDefault();
      setError('Không thể lưu mã đơn trước khi mở payOS. Vui lòng cho phép bộ nhớ phiên và thử lại.');
    }
  };

  return (
    <main className="min-h-dvh bg-stone-50 px-4 py-6 sm:py-10 text-stone-900">
      <div className="mx-auto max-w-lg space-y-5 [&_button]:min-h-11 [&_button]:focus-visible:outline-2 [&_button]:focus-visible:outline-offset-2 [&_button]:focus-visible:outline-emerald-600 [&_input]:focus-visible:outline-2 [&_input]:focus-visible:outline-emerald-600">
        <header className="text-center space-y-1">
          <p className="text-sm font-medium text-stone-500">payOS · KienlongBank</p>
          <h1 className="text-2xl font-bold">Thanh toán cho cửa hàng</h1>
          <p className="text-sm text-stone-600">Nhập số tiền bằng VND, sau đó thanh toán đơn riêng của bạn.</p>
        </header>
        <section className="rounded-3xl border border-stone-200 bg-white p-5 sm:p-7 shadow-sm space-y-5">
          {!id ? (
            <form onSubmit={submit} className="space-y-4">
              <label className="block font-semibold" htmlFor="payment-amount">Số tiền thanh toán (VND)</label>
              <input id="payment-amount" type="text" inputMode="numeric" autoComplete="off" placeholder="50000"
                value={amount} onChange={(event) => setAmount(event.target.value)} disabled={Boolean(intent) || submitting}
                className="w-full rounded-xl border border-stone-300 p-3 text-xl disabled:bg-stone-100" aria-describedby="payment-amount-help" />
              <p id="payment-amount-help" className="text-sm text-stone-600">Ví dụ: 50000 = 50.000đ. Không nhập theo nghìn đồng.</p>
              {parsedAmount !== null && <p className="font-semibold">Bạn thanh toán: {formatVndCurrency(parsedAmount)}</p>}
              {config.isPending && <p className="text-sm text-stone-600">Đang kiểm tra dịch vụ thanh toán…</p>}
              {config.isError && <div className="space-y-2 text-sm"><p>Chưa kết nối được dịch vụ thanh toán.</p><button type="button" className={secondaryClass} onClick={() => void config.refetch()}>Kiểm tra lại</button></div>}
              {config.data && !ready && <p role="status" className="text-sm text-amber-800">{readinessMessages[config.data.status]}</p>}
              {intent && !intent.orderId && <p className="text-sm text-stone-600">Đã lưu yêu cầu này. Thử lại sẽ dùng cùng mã yêu cầu, không tạo một đơn khác.</p>}
              <button type="submit" className={buttonClass} disabled={submitting || (!intent?.orderId && (!ready || parsedAmount === null))}>
                {submitting ? 'Đang tạo đơn…' : intent?.orderId ? 'Tiếp tục thanh toán' : intent ? 'Thử lại yêu cầu đã lưu' : 'Tạo đơn thanh toán'}
              </button>
            </form>
          ) : !order ? (
            <div className="space-y-3 text-center">
              <h2 className="text-lg font-semibold">{snapshot.error instanceof ApiError && snapshot.error.status === 404 ? 'Không tìm thấy đơn thanh toán' : 'Đang chờ xác nhận'}</h2>
              <p className="text-sm text-stone-600">{snapshot.error instanceof ApiError && snapshot.error.status === 404 ? 'Kiểm tra lại liên kết bạn nhận từ cửa hàng.' : 'Đang lấy trạng thái đơn. Mất mạng không có nghĩa thanh toán thất bại.'}</p>
              <button className={secondaryClass} onClick={() => void snapshot.refetch()}>Kiểm tra lại</button>
              <p><Link to="/pay" className="text-sm underline">Về trang nhập tiền</Link></p>
            </div>
          ) : (
            <div className="space-y-4">
              <div className="text-center space-y-2" role="status" aria-live="polite">
                <h2 className={`text-xl font-bold ${order.status === 'PAID' ? 'text-emerald-700' : ''}`}>{statusLabels[order.status]}</h2>
                <p className="text-3xl font-bold">{formatVndCurrency(order.amountVnd)}</p>
                <p className="text-sm text-stone-600">Mã đơn: <span className="font-mono">{order.orderCode}</span></p>
              </div>
              {order.status === 'PAID' ? (
                <div className="rounded-xl bg-emerald-50 p-4 text-emerald-900 space-y-2">
                  <p className="font-semibold">Đã nhận {formatVndCurrency(order.amountVnd)}.</p>
                  <p className="text-sm">Mã đơn: {order.orderCode}</p>
                  <p className="text-sm">Thời gian thanh toán: {order.paidAt ? formatDateTimeVN(order.paidAt) : 'Đã xác nhận'}</p>
                  <p className="text-sm">Giữ thông tin này làm biên nhận. Không thanh toán lại đơn này.</p>
                </div>
              ) : terminal ? (
                <p className="text-sm text-stone-600">QR và liên kết cũ không còn dùng để thanh toán. Bạn có thể chủ động tạo một đơn mới.</p>
              ) : expiredByClock ? (
                <p className="rounded-xl bg-amber-50 p-4 text-sm text-amber-900">QR đã hết thời gian hiển thị. Đang chờ xác nhận trạng thái từ cửa hàng; đây chưa phải xác nhận đơn bị hủy hoặc đã trả.</p>
              ) : (
                <>
                  {canPay && (qrImage ? <img src={qrImage} alt={`QR payOS thanh toán ${formatVndCurrency(order.amountVnd)}`} className="mx-auto w-full max-w-80 rounded-xl" />
                    : <p className="text-center text-sm text-stone-600">{qrError ? 'Không thể hiển thị QR. Bạn vẫn có thể dùng liên kết payOS bên dưới.' : 'Đang hiển thị QR thanh toán…'}</p>)}
                  {!order.qrCode && <p className="text-sm text-stone-600">Đang chờ thông tin thanh toán từ payOS. Không cần tạo lại đơn.</p>}
                  {order.accountNumber && <dl className="rounded-xl bg-stone-50 p-4 space-y-2 text-sm">
                    <div><dt className="text-stone-500">Ngân hàng nhận</dt><dd className="font-semibold">KienlongBank</dd></div>
                    <div><dt className="text-stone-500">Tài khoản nhận / tài khoản định danh của đơn</dt><dd className="font-mono break-all select-all">{order.accountNumber}</dd></div>
                    {order.accountName && <div><dt className="text-stone-500">Tên người nhận</dt><dd className="font-semibold">{order.accountName}</dd></div>}
                    <div><dt className="text-stone-500">Nội dung đơn</dt><dd className="font-mono select-all">DH{order.orderCode}</dd></div>
                  </dl>}
                  {order.accountNumber && <button className={`${secondaryClass} w-full`} onClick={() => void copy(`KienlongBank\n${order.accountNumber}\n${order.accountName ?? ''}\n${order.amountVnd} VND\nDH${order.orderCode}`)}>Sao chép thông tin thanh toán</button>}
                  {order.checkoutUrl && <a href={order.checkoutUrl} target="_blank" rel="noopener noreferrer" referrerPolicy="no-referrer" onClick={openCheckout} className={`${buttonClass} block text-center`}>Thanh toán trên payOS</a>}
                  <p className="text-sm text-stone-600">Dùng cùng điện thoại? Mở payOS để chọn ứng dụng ngân hàng, hoặc sao chép thông tin. Không cần quét QR trên màn hình điện thoại của bạn.</p>
                  <p className="text-xs text-stone-500">Hạn hiển thị QR: {formatDateTimeVN(order.expiresAt)}. Thanh toán đúng số tiền của đơn.</p>
                </>
              )}
              {!terminal && (!realtime.networkOnline || snapshot.isError) && <p role="status" className="text-sm text-amber-800">Đang chờ xác nhận. Kết nối bị gián đoạn; đơn và QR chưa hết hạn được giữ nguyên. Không thanh toán thêm để thử lại.</p>}
              {!terminal && <button className={`${secondaryClass} w-full`} onClick={() => void snapshot.refetch()}>Kiểm tra trạng thái</button>}
              {terminal && <button className={buttonClass} onClick={startNew}>Tạo đơn mới</button>}
            </div>
          )}
          {error && <p role="alert" className="text-sm text-red-700">{error}</p>}
          {copyMessage && <p role="status" className="text-sm text-stone-600">{copyMessage}</p>}
        </section>
        <p className="text-center text-xs text-stone-500">Trạng thái chỉ được xác nhận từ cửa hàng, không dựa vào trang quay về từ payOS.</p>
      </div>
    </main>
  );
};
