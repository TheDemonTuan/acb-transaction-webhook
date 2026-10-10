import React, { useEffect, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  CheckCircle2,
  Sparkles,
  X,
  Maximize2,
  Copy,
  Wifi,
  Store,
  CreditCard,
  Radio,
  ArrowRight,
  RefreshCw,
  Clock,
  User,
  Plus,
} from 'lucide-react';
import { fetchPaymentConfig, fetchSePayStore, fetchTransactions } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { isPublicViewerHost } from '../../app/runtime-mode';
import { useRealtimeContext } from '../../realtime/RealtimeProvider';
import type { BankTransactionCreditData } from '../../realtime/realtime.types';
import { formatVndCurrency } from '../../shared/formatters/money';
import { formatDateTimeVN } from '../../shared/formatters/datetime';
import {
  isTerminalPaymentOrder,
  loadPaymentOrderTray,
  parseCounterAmountVnd,
  type PaymentOrder,
} from './payment-orders';
import { PaymentQRImage } from './PaymentQRImage';
import { COUNTER_ARCHIVE_PAGE_SIZE, useCounterPayments } from './useCounterPayments';
import { EnlargedQRModal, type RecentCreditAlert } from './EnlargedQRModal';
import { WifiQRModal } from './WifiQRModal';

export type ActiveQR = { kind: 'store' } | { kind: 'payos'; slotId: string };
export type CashierMode = 'store' | 'payos';

const ACTIVE_QR_KEY = 'counter_active_qr_v1';
const CASHIER_MODE_KEY = 'counter_cashier_mode_v1';

const statusLabels: Record<PaymentOrder['status'], string> = {
  CREATING: 'Đang tạo đơn',
  PENDING: 'Đang chờ thanh toán',
  PROCESSING: 'Đang xử lý',
  UNDERPAID: 'Chưa đủ số tiền',
  PAID: 'Đã thanh toán',
  CANCELLED: 'Đã hủy',
  EXPIRED: 'Đã hết hạn',
  FAILED: 'Không tạo được đơn',
};

const buttonClass =
  'min-h-11 rounded-xl border border-stone-300 px-3 py-2 text-sm font-semibold focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-600 disabled:opacity-40 transition cursor-pointer';

export const CashierTerminal: React.FC = () => {
  const isPublic = isPublicViewerHost();
  const client = useQueryClient();
  const realtime = useRealtimeContext();

  const config = useQuery({
    queryKey: queryKeys.paymentConfig,
    queryFn: fetchPaymentConfig,
    refetchInterval: 30_000,
  });

  const store = useQuery({
    queryKey: queryKeys.sepayStore,
    queryFn: fetchSePayStore,
    refetchOnMount: 'always',
    refetchOnWindowFocus: 'always',
    refetchOnReconnect: 'always',
  });

  const recent = useQuery({
    queryKey: queryKeys.transactions({ direction: 'credit', limit: 10 }),
    queryFn: () => fetchTransactions({ direction: 'credit', limit: 10 }),
    refetchInterval: 15_000,
  });

  const [activeQr, setActiveQr] = useState<ActiveQR>(() => {
    try {
      const saved: unknown = JSON.parse(sessionStorage.getItem(ACTIVE_QR_KEY) ?? 'null');
      if (
        saved &&
        typeof saved === 'object' &&
        'kind' in saved &&
        saved.kind === 'payos' &&
        'slotId' in saved &&
        typeof saved.slotId === 'string'
      ) {
        const tray = loadPaymentOrderTray();
        if ([...tray.visible, ...tray.archived].some((slot) => slot.slotId === saved.slotId)) {
          return { kind: 'payos', slotId: saved.slotId };
        }
      }
    } catch {
      /* Selection persistence is optional */
    }
    return { kind: 'store' };
  });

  const [mode, setMode] = useState<CashierMode>(() => {
    try {
      const savedMode = sessionStorage.getItem(CASHIER_MODE_KEY);
      if (savedMode === 'payos' || savedMode === 'store') return savedMode;
    } catch {
      /* ignore */
    }
    return activeQr.kind === 'payos' ? 'payos' : 'store';
  });

  const [archivePage, setArchivePage] = useState<number | null>(null);
  const payments = useCounterPayments(
    isPublic,
    activeQr.kind === 'payos' ? activeQr.slotId : undefined,
    archivePage
  );

  const [amount, setAmount] = useState(() => {
    try {
      return sessionStorage.getItem('counter_draft_amount_v1') ?? '';
    } catch {
      return '';
    }
  });
  const [name, setName] = useState('');
  const [nameOpen, setNameOpen] = useState(
    () => typeof window !== 'undefined' && window.matchMedia('(min-width: 1024px)').matches
  );
  const [now, setNow] = useState(Date.now());
  const [removeConfirmation, setRemoveConfirmation] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState<string | null>(null);
  const [enlarged, setEnlarged] = useState(false);
  const [wifiOpen, setWifiOpen] = useState(false);
  const [copyNotice, setCopyNotice] = useState('');
  const [latestCredit, setLatestCredit] = useState<RecentCreditAlert | null>(null);

  const amountRef = useRef<HTMLInputElement>(null);
  const enlargeButtonRef = useRef<HTMLButtonElement>(null);
  const submitLocked = useRef(false);
  const interaction = useRef({ amount: 0, name: 0, selection: 0, focus: 0 });

  const activeSlot =
    activeQr.kind === 'payos'
      ? payments.allSlots.find((slot) => slot.slotId === activeQr.slotId)
      : undefined;
  const order = activeSlot ? payments.orders[activeSlot.slotId] : undefined;
  const expired = !!order && Date.parse(order.expiresAt) <= now;
  const payable = !!order && !expired && !isTerminalPaymentOrder(order.status);
  const storeReady = store.data?.status === 'ACTIVE' && !!store.data.qrPayload;
  const preview = parseCounterAmountVnd(amount, config.data?.maxAmountVnd);
  const payload =
    activeQr.kind === 'store'
      ? storeReady
        ? store.data!.qrPayload
        : ''
      : payable
      ? order!.qrCode ?? ''
      : '';

  const select = (next: ActiveQR) => {
    interaction.current.selection++;
    setActiveQr(next);
    setCopyNotice('');
  };

  const handleSwitchMode = (nextMode: CashierMode) => {
    setMode(nextMode);
    try {
      sessionStorage.setItem(CASHIER_MODE_KEY, nextMode);
    } catch {
      /* ignore */
    }
    if (nextMode === 'store') {
      select({ kind: 'store' });
    } else if (nextMode === 'payos') {
      if (payments.slots.length > 0 && activeQr.kind !== 'payos') {
        select({ kind: 'payos', slotId: payments.slots[0].slotId });
      }
      setTimeout(() => amountRef.current?.focus({ preventScroll: true }), 100);
    }
  };

  useEffect(() => {
    try {
      sessionStorage.setItem(ACTIVE_QR_KEY, JSON.stringify(activeQr));
    } catch {
      /* ignore */
    }
  }, [activeQr]);
  useEffect(() => {
    try {
      if (amount) sessionStorage.setItem('counter_draft_amount_v1', amount);
      else sessionStorage.removeItem('counter_draft_amount_v1');
    } catch {
      /* ignore */
    }
  }, [amount]);

  useEffect(() => {
    if (activeQr.kind === 'payos' && !payments.allSlots.some((slot) => slot.slotId === activeQr.slotId)) {
      setActiveQr({ kind: 'store' });
    }
  }, [activeQr, payments.allSlots]);

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    const recover = () => {
      void client.invalidateQueries({ queryKey: queryKeys.sepayStore });
      void client.invalidateQueries({ queryKey: queryKeys.transactions() });
    };
    if (realtime.status === 'CONNECTED') recover();
    window.addEventListener('focus', recover);
    window.addEventListener('online', recover);
    window.addEventListener('pageshow', recover);
    return () => {
      window.removeEventListener('focus', recover);
      window.removeEventListener('online', recover);
      window.removeEventListener('pageshow', recover);
    };
  }, [client, realtime.status, realtime.reconnectCount]);

  useEffect(() => {
    return realtime.subscribe<BankTransactionCreditData>('bank.transaction.credit', ({ data }) => {
      if (!data) return;
      setLatestCredit({
        id: data.transactionId,
        amountVnd: Number(data.credit || 0),
        bank: data.bank,
        provider: data.provider,
        orderCode: data.orderCode,
        time: formatDateTimeVN(data.transactionDate || new Date().toISOString()),
        reference: data.transactionNumber,
        timestamp: Date.now(),
      });
      void client.invalidateQueries({ queryKey: queryKeys.transactions() });
    });
  }, [realtime.subscribe, client]);

  const nextCustomer = () => {
    select({ kind: 'store' });
    setAmount('');
    setName('');
    if (window.matchMedia('(min-width: 1024px)').matches) {
      amountRef.current?.focus({ preventScroll: true });
    }
  };

  const archiveSlot = async (slotId: string) => {
    interaction.current.selection++;
    if (!(await payments.removeSlot(slotId))) return;
    setActiveQr((current) =>
      current.kind === 'payos' && current.slotId === slotId ? { kind: 'store' } : current
    );
    setRemoveConfirmation(null);
  };

  const handleCopyInfo = () => {
    const value =
      activeQr.kind === 'store'
        ? store.data
          ? `${store.data.bank} · ${store.data.accountName}\nSTK: ${store.data.accountNumber}`
          : ''
        : order
        ? `${order.accountNumber ?? ''}\n${order.amountVnd} VND\nDH${order.orderCode}`
        : '';
    if (!value) return;
    void navigator.clipboard.writeText(value).then(
      () => setCopyNotice('Đã sao chép thông tin nhận tiền'),
      () => setCopyNotice('Không thể sao chép.')
    );
  };

  const selectedContent = (
    <>
      <p className="text-xs sm:text-sm font-bold text-emerald-800">
        {activeQr.kind === 'store'
          ? 'QR chuyển khoản cửa hàng · SePay'
          : `payOS · ${activeSlot?.name ?? 'Đơn thanh toán'}`}
      </p>
      <p className="text-xl sm:text-2xl font-black text-stone-900 tracking-tight">
        {activeQr.kind === 'store'
          ? 'Khách tự nhập số tiền'
          : activeSlot
          ? formatVndCurrency(activeSlot.amountVnd)
          : ''}
      </p>

      {activeQr.kind === 'store' ? (
        storeReady ? (
          <>
            <PaymentQRImage payload={payload} alt="QR chuyển khoản cửa hàng SePay" />
            <div className="space-y-0.5 text-center">
              <p className="font-bold text-stone-900 text-sm sm:text-base">{store.data!.storeName}</p>
              <p className="text-xs text-stone-600">
                {store.data!.bank} · <span className="font-semibold">{store.data!.accountName}</span>
              </p>
              <p className="font-mono text-sm sm:text-base font-bold text-stone-800 break-all select-all tracking-wider">
                {store.data!.accountNumber}
              </p>
              <p className="text-xs text-stone-500 pt-1">Quét bằng ứng dụng ngân hàng</p>
            </div>
          </>
        ) : (
          <div className="w-full rounded-2xl bg-stone-50 p-6 text-center space-y-2">
            <p className="font-semibold text-stone-800">QR cửa hàng chưa sẵn sàng</p>
            <p className="text-xs text-stone-600">
              {store.isPending ? 'Đang tải cấu hình cửa hàng…' : 'Chuyển sang tab payOS để tạo mã có số tiền.'}
            </p>
            <button
              type="button"
              className="text-xs font-semibold text-emerald-700 underline"
              onClick={() => void store.refetch()}
            >
              Kiểm tra lại QR cửa hàng
            </button>
          </div>
        )
      ) : order?.status === 'PAID' ? (
        <div className="w-full rounded-2xl bg-emerald-50 p-5 text-emerald-900 space-y-2 text-center">
          <div className="w-10 h-10 bg-emerald-100 rounded-full flex items-center justify-center mx-auto text-emerald-700">
            <CheckCircle2 className="w-6 h-6" />
          </div>
          <h3 className="text-lg font-bold">Thanh toán thành công</h3>
          <p className="text-sm font-semibold">Đã nhận {formatVndCurrency(order.amountVnd)}</p>
          <p className="text-xs text-stone-600">Mã đơn: {order.orderCode}</p>
          {order.paidAt && <p className="text-xs text-stone-500">{formatDateTimeVN(order.paidAt)}</p>}
          <p className="text-[11px] text-emerald-800 font-medium">Giữ biên nhận. Không thanh toán lại đơn này.</p>
        </div>
      ) : order && (expired || isTerminalPaymentOrder(order.status)) ? (
        <div className="min-h-56 flex items-center justify-center rounded-2xl bg-amber-50 p-5 text-xs text-amber-900 text-center">
          {expired && !isTerminalPaymentOrder(order.status)
            ? 'Đã hết thời gian hiển thị QR. Đang chờ xác nhận trạng thái từ máy chủ.'
            : `${statusLabels[order.status]}. QR này không còn dùng để thanh toán.`}
        </div>
      ) : payable ? (
        <>
          {payload ? (
            <PaymentQRImage payload={payload} alt={`QR thanh toán ${activeSlot!.name}`} />
          ) : (
            <p className="min-h-56 flex items-center text-xs text-stone-500">Đang chờ mã QR từ payOS...</p>
          )}
          <div className="space-y-0.5 text-center">
            <p className="font-bold text-stone-900 text-sm">KienlongBank · {order!.accountName}</p>
            <p className="font-mono text-sm font-bold text-stone-800 break-all select-all">
              {order!.accountNumber}
            </p>
            <p className="text-xs text-stone-600">Mã đơn: {order!.orderCode}</p>
            <p className="text-[11px] text-stone-400">Hạn: {formatDateTimeVN(order!.expiresAt)}</p>
            {order!.checkoutUrl && (
              <a
                href={order!.checkoutUrl}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 text-xs font-semibold text-emerald-700 underline mt-1"
              >
                Mở trang thanh toán payOS
              </a>
            )}
          </div>
        </>
      ) : (
        <div className="min-h-56 flex items-center justify-center rounded-2xl bg-stone-50 p-5 text-xs font-semibold text-stone-600 text-center">
          {activeSlot && payments.creatingKeys.includes(activeSlot.idempotencyKey)
            ? 'Đang tạo QR payOS...'
            : 'Nhập số tiền ở cột bên phải để tạo mã QR'}
        </div>
      )}
    </>
  );

  return (
    <section
      id="counter-checkout"
      aria-label="Thu ngân"
      onFocusCapture={() => {
        interaction.current.focus++;
      }}
      className="scroll-mt-4 space-y-4 [&_button]:cursor-pointer [&_button]:focus-visible:outline-2 [&_button]:focus-visible:outline-offset-2 [&_button]:focus-visible:outline-emerald-600 [&_input]:focus-visible:outline-2 [&_input]:focus-visible:outline-emerald-600"
    >
      {/* Realtime Instant Credit Flash Banner */}
      {latestCredit && (
        <div
          role="status"
          aria-live="polite"
          className="rounded-2xl bg-gradient-to-r from-emerald-600 to-emerald-700 text-white p-4 shadow-lg flex items-center justify-between gap-3 animate-in fade-in slide-in-from-top-2 border-2 border-emerald-400/50"
        >
          <div className="flex items-center gap-3 min-w-0">
            <div className="w-10 h-10 rounded-xl bg-white/20 flex items-center justify-center shrink-0 text-xl font-bold">
              ✓
            </div>
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <span className="font-bold text-xs uppercase tracking-wider text-emerald-100 flex items-center gap-1">
                  <Sparkles className="w-3.5 h-3.5 text-yellow-300" />
                  Vừa nhận tiền thành công!
                </span>
                <span className="text-xs bg-white/20 px-2 py-0.5 rounded-full">{latestCredit.time}</span>
              </div>
              <p className="text-2xl font-black mt-0.5 tracking-tight">
                +{formatVndCurrency(latestCredit.amountVnd)}
              </p>
              <p className="text-xs text-emerald-100 truncate mt-0.5">
                {latestCredit.provider === 'SEPAY' ? 'SePay Store' : 'payOS'} ·{' '}
                {latestCredit.bank || 'Ngân hàng'} · Mã:{' '}
                {latestCredit.reference || latestCredit.orderCode || latestCredit.id}
              </p>
            </div>
          </div>
          <button
            type="button"
            onClick={() => setLatestCredit(null)}
            className="min-h-11 min-w-11 flex items-center justify-center text-white/80 hover:text-white hover:bg-white/10 rounded-xl transition cursor-pointer shrink-0"
            title="Đóng thông báo"
            aria-label="Đóng thông báo"
          >
            <X className="w-5 h-5" />
          </button>
        </div>
      )}

      {/* POS Toolbar & Mode Switcher Header */}
      <div className="bg-white p-3 sm:p-4 rounded-2xl border border-stone-200/80 shadow-xs flex flex-col md:flex-row md:items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-2.5">
          <div className="flex items-center gap-2">
            <h2 className="text-lg sm:text-xl font-black tracking-tight text-stone-900">
              Quầy thu ngân
            </h2>
            <span className="inline-flex items-center gap-1.5 px-2.5 py-0.5 rounded-full text-xs font-semibold bg-emerald-50 text-emerald-800 border border-emerald-200">
              <span className="w-2 h-2 rounded-full bg-emerald-500 animate-pulse" />
              <span>Realtime</span>
            </span>
          </div>

          {/* Mode Selector Segmented Pills */}
          <div
            className="flex items-center bg-stone-100 p-1 rounded-xl text-xs font-bold"
            aria-label="Chế độ thu ngân"
          >
            <button
              type="button"
              aria-pressed={mode === 'store'}
              onClick={() => handleSwitchMode('store')}
              className={`flex items-center gap-1.5 px-3 py-1.5 rounded-lg transition cursor-pointer ${
                mode === 'store'
                  ? 'bg-emerald-700 text-white shadow-xs'
                  : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              <Store className="w-3.5 h-3.5" />
              <span>Chế độ SePay Store</span>
            </button>
            <button
              type="button"
              aria-pressed={mode === 'payos'}
              onClick={() => handleSwitchMode('payos')}
              className={`flex items-center gap-1.5 px-3 py-1.5 rounded-lg transition cursor-pointer ${
                mode === 'payos'
                  ? 'bg-emerald-700 text-white shadow-xs'
                  : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              <CreditCard className="w-3.5 h-3.5" />
              <span>Chế độ payOS Đơn lẻ</span>
            </button>
          </div>
        </div>

        {/* Right utility & Connection status */}
        <div className="flex flex-wrap items-center gap-2 self-start md:self-auto">
          {/* Realtime & SePay Live Connection Status Badge */}
          <div className="inline-flex items-center gap-2 px-3 py-1.5 rounded-xl bg-stone-50 border border-stone-200/90 text-xs text-stone-600 shadow-2xs">
            <span
              className={`w-2 h-2 rounded-full shrink-0 ${
                realtime.status === 'CONNECTED' ? 'bg-emerald-500 animate-pulse' : 'bg-amber-500'
              }`}
            />
            <span className="font-semibold text-stone-800">
              {realtime.status === 'CONNECTED' ? 'Đã kết nối' : 'Đang kết nối lại'}
            </span>
            <span className="text-stone-300">·</span>
            <span>
              SePay:{' '}
              <strong
                className={
                  store.data?.status === 'ACTIVE'
                    ? 'text-emerald-700 font-semibold'
                    : 'text-stone-700 font-semibold'
                }
              >
                {store.data?.status === 'ACTIVE'
                  ? 'Đã bật nhận thông báo'
                  : store.data?.status === 'OBSERVING'
                  ? 'Đang quan sát'
                  : 'Chưa bật'}
              </strong>
            </span>
            {store.data?.lastMessageAt && (
              <>
                <span className="text-stone-300 hidden lg:inline">·</span>
                <span className="text-stone-500 hidden lg:inline">
                  Lần nhận cuối: {formatDateTimeVN(store.data.lastMessageAt)}
                </span>
              </>
            )}
            <button
              type="button"
              className="ml-1 text-[11px] font-bold text-emerald-800 hover:text-emerald-900 underline cursor-pointer"
              onClick={realtime.forceReconnect}
              title="Bấm để kết nối lại máy chủ realtime"
            >
              Kết nối lại
            </button>
          </div>

          <button
            type="button"
            onClick={() => setWifiOpen(true)}
            className="min-h-9 inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl border border-stone-200 bg-stone-50 hover:bg-stone-100 text-stone-700 text-xs font-semibold transition cursor-pointer"
            title="Xem mã kết nối WiFi quán"
          >
            <Wifi className="w-4 h-4 text-blue-600" />
            <span>WiFi quán</span>
          </button>
        </div>
      </div>

      {(!realtime.networkOnline || !realtime.serverReachable || realtime.status !== 'CONNECTED') && (
        <p role="status" className="rounded-xl bg-amber-50 px-3 py-2 text-sm text-amber-900">
          Đang mất cập nhật — kiểm tra nhận tiền trước khi giao hàng
        </p>
      )}

      {/* Main 2-Column POS Layout */}
      <div className="grid min-w-0 items-start gap-4 md:grid-cols-[340px_minmax(0,1fr)] lg:grid-cols-[400px_minmax(0,1fr)]">
        {/* CỘT TRÁI: QR CODE & THÔNG TIN THANH TOÁN */}
        <div className="min-w-0 space-y-3">
          <div className="flex items-center gap-2" aria-label="Chọn QR">
            <button
              type="button"
              aria-pressed={activeQr.kind === 'store'}
              className={`${buttonClass} ${
                activeQr.kind === 'store'
                  ? 'bg-emerald-50 border-emerald-600 text-emerald-900'
                  : 'bg-white text-stone-700'
              }`}
              onClick={() => select({ kind: 'store' })}
            >
              QR cửa hàng · SePay
            </button>
            {activeQr.kind === 'payos' && (
              <span className="min-w-0 truncate text-xs sm:text-sm font-bold text-stone-800">
                payOS · {activeSlot?.name}
              </span>
            )}
          </div>
          {/* Active QR display card */}
          <div
            data-testid="counter-active-qr"
            data-qr-kind={activeQr.kind}
            data-slot-id={activeQr.kind === 'payos' ? activeQr.slotId : undefined}
            className="min-w-0 rounded-2xl border border-stone-200/90 bg-white p-4 sm:p-5 text-center flex flex-col items-center gap-3 shadow-xs"
          >
            <div
              aria-hidden={enlarged}
              className={`flex w-full flex-col items-center gap-2.5 ${enlarged ? 'invisible' : ''}`}
            >
              {selectedContent}
            </div>

            {(payload || order) && (
              <div className="flex flex-wrap justify-center gap-2 w-full pt-1">
                <button
                  ref={enlargeButtonRef}
                  type="button"
                  className={`${buttonClass} flex-1 inline-flex items-center justify-center gap-1.5 bg-stone-900 text-white border-stone-900 hover:bg-stone-800`}
                  onClick={() => setEnlarged(true)}
                >
                  <Maximize2 className="w-4 h-4" />
                  <span>Phóng to QR</span>
                </button>
                {payload && (
                  <button
                    type="button"
                    className={`${buttonClass} inline-flex items-center justify-center gap-1.5 bg-stone-50 hover:bg-stone-100 text-stone-700`}
                    onClick={handleCopyInfo}
                    title="Sao chép thông tin nhận tiền"
                  >
                    <Copy className="w-4 h-4" />
                    <span>Sao chép</span>
                  </button>
                )}
              </div>
            )}

            {copyNotice && <p role="status" className="text-xs font-semibold text-emerald-700">{copyNotice}</p>}

            {activeSlot && !activeSlot.orderId && (
              <button
                type="button"
                className={`${buttonClass} w-full text-emerald-800 border-emerald-300 bg-emerald-50/50`}
                disabled={payments.creatingKeys.includes(activeSlot.idempotencyKey)}
                onClick={() => payments.retrySlot(activeSlot)}
              >
                Thử lại cùng đơn
              </button>
            )}

            {activeSlot && payments.slotErrors[activeSlot.slotId] && (
              <p role="alert" className="text-xs text-amber-900">
                {payments.slotErrors[activeSlot.slotId]}
              </p>
            )}
          </div>

          {/* Quick Action Button below QR */}
          <button
            type="button"
            className={`${buttonClass} w-full bg-white hover:bg-stone-50 text-stone-700 shadow-2xs`}
            onClick={nextCustomer}
          >
            Khách tiếp theo
          </button>
        </div>

        {/* CỘT PHẢI: TÙY THEO CHẾ ĐỘ SEPAY HOẶC PAYOS */}
        <div className="min-w-0 space-y-4">
          {/* === KHI Ở CHẾ ĐỘ SEPAY: TOÀN BỘ CHIỀU CAO CHO BẢNG GIAO DỊCH TIỀN VÀO TRỰC TIẾP === */}
          {mode === 'store' && (
            <section
              aria-label="Giao dịch vừa nhận"
              className="rounded-2xl border border-stone-200/90 bg-white p-4 sm:p-5 space-y-3.5 shadow-xs"
            >
              <div className="flex items-center justify-between pb-3 border-b border-stone-100">
                <div className="flex items-center gap-2.5">
                  <span className="w-2.5 h-2.5 rounded-full bg-emerald-500 animate-pulse" />
                  <div>
                    <h3 className="font-extrabold text-stone-900 text-base leading-tight">
                      Giao dịch vừa nhận (Trực tiếp)
                    </h3>
                    <p className="text-xs text-stone-500">
                      Tự động ghi nhận tiền vào SePay Store không cần tạo đơn
                    </p>
                  </div>
                </div>
                <div className="flex items-center gap-2">
                  <span className="text-[11px] font-bold text-emerald-800 bg-emerald-50 border border-emerald-200 px-2.5 py-1 rounded-full">
                    ● Realtime Live
                  </span>
                  <button
                    type="button"
                    onClick={() => void recent.refetch()}
                    disabled={recent.isRefetching}
                    className="p-1 text-stone-400 hover:text-stone-700 transition"
                    title="Làm mới danh sách"
                  >
                    <RefreshCw className={`w-3.5 h-3.5 ${recent.isRefetching ? 'animate-spin' : ''}`} />
                  </button>
                </div>
              </div>

              {/* Transactions List */}
              {recent.data?.items && recent.data.items.length > 0 ? (
                <div className="space-y-2.5 max-h-[580px] overflow-y-auto pr-1">
                  {recent.data.items.map((tx) => {
                    const isVeryRecent =
                      Date.now() - Date.parse(tx.firstSeenAt || tx.transactionDate || '') < 180_000;
                    return (
                      <div
                        key={tx.id}
                        className={`p-3.5 rounded-xl border transition flex items-center justify-between gap-3 ${
                          isVeryRecent
                            ? 'border-emerald-300 bg-emerald-50/70 shadow-2xs ring-1 ring-emerald-400'
                            : 'border-stone-100 bg-stone-50/70 hover:bg-stone-50'
                        }`}
                      >
                        <div className="min-w-0 flex-1">
                          <div className="flex flex-wrap items-center gap-2">
                            <strong className="text-emerald-700 font-black text-lg">
                              +{formatVndCurrency(Number(tx.credit))}
                            </strong>
                            <span
                              className={`text-[10px] font-bold px-2 py-0.5 rounded-md ${
                                tx.provider === 'SEPAY'
                                  ? 'bg-emerald-100 text-emerald-800 border border-emerald-200'
                                  : 'bg-blue-100 text-blue-800 border border-blue-200'
                              }`}
                            >
                              {tx.provider === 'SEPAY'
                                ? 'SePay Store'
                                : tx.provider === 'PAYOS'
                                ? `payOS · ${tx.orderCode ?? ''}`
                                : tx.bank ?? 'ACB'}
                            </span>
                            {isVeryRecent && (
                              <span className="text-[9px] font-bold text-white bg-emerald-600 px-2 py-0.5 rounded-full animate-pulse">
                                VỪA NHẬN
                              </span>
                            )}
                          </div>
                          <p className="text-xs text-stone-600 mt-1 truncate">
                            {tx.bank ? `${tx.bank} · ` : ''}
                            {formatDateTimeVN(tx.transactionDate ?? tx.firstSeenAt)}
                          </p>
                          {tx.description && (
                            <p className="text-xs font-medium text-stone-900 mt-0.5 truncate">
                              {tx.description}
                            </p>
                          )}
                        </div>
                      </div>
                    );
                  })}
                </div>
              ) : (
                <div className="py-16 text-center text-stone-500 text-xs space-y-1">
                  <p className="font-semibold text-stone-700">Chưa có giao dịch tiền vào hôm nay</p>
                  <p className="text-stone-400">
                    Khi khách chuyển khoản qua mã QR SePay, thông báo tiền vào sẽ xuất hiện ngay tại đây.
                  </p>
                </div>
              )}

              {/* Quick switch prompt for cashier */}
              <div className="pt-2 border-t border-stone-100 flex items-center justify-between text-xs text-stone-500">
                <span>Cần tạo đơn có số tiền cụ thể?</span>
                <button
                  type="button"
                  onClick={() => handleSwitchMode('payos')}
                  className="font-bold text-emerald-700 hover:text-emerald-800 inline-flex items-center gap-1 cursor-pointer"
                >
                  <span>Chuyển sang QR payOS</span>
                  <ArrowRight className="w-3.5 h-3.5" />
                </button>
              </div>
            </section>
          )}

          {/* === KHI Ở CHẾ ĐỘ PAYOS: FORM NHẬP TIỀN + KHAY KHÁCH + GIAO DỊCH GẦN ĐÂY === */}
          {mode === 'payos' && (
            <div className="space-y-4">
              {/* Form nhập tiền tạo đơn payOS */}
              <form
                className="min-w-0 rounded-2xl border border-stone-200/90 bg-white p-4 sm:p-5 space-y-3 shadow-xs"
                onSubmit={async (event) => {
                  event.preventDefault();
                  if (submitLocked.current) return;
                  submitLocked.current = true;
                  const submitted = { ...interaction.current };
                  try {
                    const slot = await payments.addSlot(amount, name);
                    if (slot) {
                      const unchangedSelection = interaction.current.selection === submitted.selection;
                      const unchangedAmount = interaction.current.amount === submitted.amount;
                      const unchangedName = interaction.current.name === submitted.name;
                      const unchangedFocus = interaction.current.focus === submitted.focus;
                      if (unchangedSelection) select({ kind: 'payos', slotId: slot.slotId });
                      if (unchangedAmount) setAmount('');
                      if (unchangedName) setName('');
                      if (unchangedSelection && unchangedAmount && unchangedName && unchangedFocus) {
                        amountRef.current?.focus({ preventScroll: true });
                      }
                    }
                  } finally {
                    submitLocked.current = false;
                  }
                }}
              >
                <div className="flex items-center justify-between">
                  <label htmlFor="counter-amount" className="block text-xs font-bold uppercase tracking-wider text-stone-600">
                    Số tiền · nghìn đồng
                  </label>
                  <span className="text-xs text-stone-500">Enter để tạo</span>
                </div>

                <div className="flex gap-2">
                  <input
                    ref={amountRef}
                    id="counter-amount"
                    type="text"
                    inputMode="numeric"
                    autoComplete="off"
                    value={amount}
                    onChange={(event) => {
                      interaction.current.amount++;
                      setAmount(event.target.value);
                    }}
                    placeholder="50 = 50.000đ"
                    aria-describedby="counter-amount-preview"
                    className="min-h-12 min-w-0 w-full rounded-xl border border-stone-300 px-3.5 text-2xl font-black text-stone-900"
                  />
                  <button
                    type="submit"
                    disabled={preview === null || !config.data?.ready || !realtime.networkOnline}
                    className={`${buttonClass} shrink-0 bg-emerald-700 text-white border-emerald-700 hover:bg-emerald-800 font-bold px-4`}
                  >
                    Tạo QR payOS
                  </button>
                </div>

                {/* Quick amount presets */}
                <div className="flex flex-wrap items-center gap-1.5 pt-1">
                  <span className="text-xs text-stone-500 mr-1">Gợi ý:</span>
                  {[20, 30, 50, 100, 200, 500].map((preset) => (
                    <button
                      key={preset}
                      type="button"
                      onClick={() => {
                        interaction.current.amount++;
                        setAmount(String(preset));
                        amountRef.current?.focus({ preventScroll: true });
                      }}
                      className="px-2.5 py-1 rounded-lg text-xs font-semibold bg-stone-100 hover:bg-stone-200/80 text-stone-700 transition"
                    >
                      {preset}k
                    </button>
                  ))}
                </div>

                <div className="flex flex-wrap items-center justify-between gap-2 pt-1">
                  <p id="counter-amount-preview" className="text-base font-extrabold text-emerald-800">
                    {preview === null ? '50 → 50.000đ' : formatVndCurrency(preview)}
                  </p>
                  <span className="text-xs text-stone-500">Tạo mã riêng có số tiền cho khách</span>
                </div>

                <details
                  className="text-xs text-stone-600 border-t border-stone-100 pt-2"
                  open={nameOpen || Boolean(name)}
                  onToggle={(e) => setNameOpen(e.currentTarget.open)}
                >
                  <summary className="min-h-9 cursor-pointer py-1 font-medium hover:text-stone-900">
                    Thêm tên khách
                  </summary>
                  <label className="block mt-2">
                    Tên khách (tùy chọn)
                    <input
                      value={name}
                      onChange={(event) => {
                        interaction.current.name++;
                        setName(event.target.value);
                      }}
                      placeholder="Ví dụ: Bàn 3, Anh Nam..."
                      maxLength={80}
                      className="mt-1 block min-h-10 w-full rounded-xl border border-stone-300 px-3 text-xs text-stone-900"
                    />
                  </label>
                </details>

                {config.data && !config.data.ready && (
                  <p className="text-xs text-amber-900">
                    payOS chưa sẵn sàng nhận đơn mới. QR cửa hàng và các đơn hiện có vẫn được giữ nguyên.
                  </p>
                )}
                {config.isError && (
                  <p role="alert" className="text-xs text-amber-900">
                    Chưa tải được cấu hình payOS. Không tạo đơn mới khi chưa xác nhận dịch vụ.
                  </p>
                )}
                {payments.notice && (
                  <p role="alert" className="text-xs text-amber-900">
                    {payments.notice}
                  </p>
                )}
              </form>

              {/* Khay khách gần đây */}
              <div className="min-w-0 space-y-2 rounded-2xl border border-stone-200/90 bg-white p-4 shadow-xs">
                <div className="flex items-center justify-between gap-2 pb-2 border-b border-stone-100">
                  <h3 className="text-sm font-bold text-stone-900">Khay khách gần đây</h3>
                  <button
                    type="button"
                    aria-expanded={archivePage !== null}
                    aria-controls="counter-archive"
                    className="text-xs font-semibold text-stone-600 hover:text-stone-900 underline"
                    onClick={() => setArchivePage((current) => (current === null ? 0 : null))}
                  >
                    Đơn trước ({payments.archived.length})
                  </button>
                </div>

                <div className="grid min-w-0 gap-2 sm:grid-cols-3" aria-label="Khay khách">
                  {payments.slots.map((slot) => {
                    const snapshot = payments.orders[slot.slotId];
                    const creating = payments.creatingKeys.includes(slot.idempotencyKey);
                    const isSelected = activeQr.kind === 'payos' && activeQr.slotId === slot.slotId;
                    return (
                      <article
                        aria-label={slot.name}
                        key={slot.slotId}
                        className={`min-w-0 rounded-xl border p-2.5 transition ${
                          snapshot?.status === 'PAID'
                            ? 'border-emerald-400 bg-emerald-50/40'
                            : isSelected
                            ? 'border-emerald-700 bg-emerald-50/20 ring-1 ring-emerald-600'
                            : 'border-stone-200 bg-white hover:border-stone-300'
                        }`}
                      >
                        <button
                          type="button"
                          className="min-h-10 w-full text-left"
                          aria-label={`Hiện QR ${slot.name}`}
                          aria-pressed={isSelected}
                          onClick={() => select({ kind: 'payos', slotId: slot.slotId })}
                        >
                          <span className="block truncate text-xs font-medium text-stone-600">{slot.name}</span>
                          <strong className="text-base font-extrabold text-emerald-800">
                            {formatVndCurrency(slot.amountVnd)}
                          </strong>
                        </button>
                        <p role="status" aria-live="polite" className="text-[11px] font-semibold text-stone-700 mt-1">
                          {snapshot ? statusLabels[snapshot.status] : creating ? 'Đang tạo đơn' : 'Đang chờ xác nhận'}
                        </p>
                        <details className="text-xs mt-1 border-t border-stone-100 pt-1">
                          <summary className="cursor-pointer py-1 text-stone-500 hover:text-stone-800">
                            Thao tác đơn
                          </summary>
                          {snapshot && <p className="break-all text-[11px] text-stone-500">Mã: {snapshot.orderCode}</p>}
                          <div className="space-y-1 pt-1">
                            <button
                              type="button"
                              className="w-full text-left underline text-emerald-800"
                              onClick={() => select({ kind: 'payos', slotId: slot.slotId })}
                            >
                              Hiện QR
                            </button>
                            {!slot.orderId && (
                              <button
                                type="button"
                                className={`${buttonClass} w-full text-xs`}
                                disabled={creating}
                                onClick={() => payments.retrySlot(slot)}
                              >
                                Thử lại cùng đơn
                              </button>
                            )}
                            {slot.orderId && (
                              <button
                                type="button"
                                className="w-full text-left underline text-stone-600"
                                onClick={payments.refreshOrders}
                              >
                                Làm mới trạng thái
                              </button>
                            )}
                            {payments.slotErrors[slot.slotId] && (
                              <p role="alert" className="text-xs text-amber-900">
                                {payments.slotErrors[slot.slotId]}
                              </p>
                            )}
                            {snapshot?.status === 'PAID' ? (
                              <button
                                type="button"
                                className="w-full text-left underline text-stone-600"
                                onClick={() => void archiveSlot(slot.slotId)}
                              >
                                Xong, bỏ khỏi khay
                              </button>
                            ) : removeConfirmation === slot.slotId ? (
                              <div className="rounded-xl bg-amber-50 p-2 text-xs space-y-1">
                                <p>Bỏ khỏi khay không hủy đơn</p>
                                <button
                                  type="button"
                                  className="underline mr-2 font-bold text-amber-900"
                                  onClick={() => void archiveSlot(slot.slotId)}
                                >
                                  Xác nhận bỏ khỏi khay
                                </button>
                                <button
                                  type="button"
                                  className="underline text-stone-600"
                                  onClick={() => setRemoveConfirmation(null)}
                                >
                                  Giữ lại
                                </button>
                              </div>
                            ) : (
                              <button
                                type="button"
                                className="w-full text-left underline text-stone-500"
                                onClick={() => setRemoveConfirmation(slot.slotId)}
                              >
                                Bỏ khỏi khay
                              </button>
                            )}
                            {!isPublic && snapshot && !isTerminalPaymentOrder(snapshot.status) && (
                              <button
                                type="button"
                                disabled={cancelling === slot.slotId}
                                className="w-full text-left text-xs font-semibold text-rose-800 pt-1"
                                onClick={() => {
                                  setCancelling(slot.slotId);
                                  void payments.cancelSlot(slot.slotId).finally(() => setCancelling(null));
                                }}
                              >
                                {cancelling === slot.slotId ? 'Đang hủy…' : 'Hủy đơn trên payOS'}
                              </button>
                            )}
                          </div>
                        </details>
                      </article>
                    );
                  })}
                </div>

                {archivePage !== null && (
                  <section
                    id="counter-archive"
                    aria-label="Đơn trước"
                    className="max-h-80 overflow-y-auto rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2 mt-2"
                  >
                    <div className="flex items-center justify-between">
                      <h4 className="font-semibold text-xs text-stone-800">
                        Đơn trước · không hủy khi lưu trữ
                      </h4>
                      <button
                        type="button"
                        className="text-xs underline text-stone-600"
                        onClick={() => setArchivePage(null)}
                      >
                        Đóng đơn trước
                      </button>
                    </div>
                    {payments.archivedPage.map((slot) => {
                      const snapshot = payments.orders[slot.slotId];
                      const creating = payments.creatingKeys.includes(slot.idempotencyKey);
                      return (
                        <article
                          key={slot.slotId}
                          aria-label={slot.name}
                          className="rounded-lg border border-stone-200 bg-white p-2 text-xs space-y-1"
                        >
                          <div className="flex flex-wrap items-center justify-between gap-1">
                            <strong>{slot.name} · {formatVndCurrency(slot.amountVnd)}</strong>
                            <span role="status" className="text-stone-600">
                              {snapshot
                                ? statusLabels[snapshot.status]
                                : creating
                                ? 'Đang tạo đơn'
                                : slot.status
                                ? `Lần xác nhận trước: ${statusLabels[slot.status]}`
                                : 'Chưa xác nhận'}
                            </span>
                          </div>
                          {(snapshot?.orderCode ?? slot.orderCode) && (
                            <p className="text-[11px] text-stone-500">Mã đơn: {snapshot?.orderCode ?? slot.orderCode}</p>
                          )}
                          <div className="flex flex-wrap gap-2 pt-1">
                            <button
                              type="button"
                              className="underline text-emerald-800"
                              onClick={() => select({ kind: 'payos', slotId: slot.slotId })}
                            >
                              Xem đơn / QR
                            </button>
                            <button
                              type="button"
                              className="underline text-stone-600"
                              onClick={() => payments.restoreSlot(slot.slotId)}
                            >
                              Đưa lại khay
                            </button>
                            {!slot.orderId && (
                              <button
                                type="button"
                                className="underline text-stone-600"
                                disabled={creating}
                                onClick={() => payments.retrySlot(slot)}
                              >
                                Thử lại cùng đơn
                              </button>
                            )}
                            {slot.orderId && (
                              <button
                                type="button"
                                className="underline text-stone-600"
                                onClick={payments.refreshOrders}
                              >
                                Làm mới trạng thái
                              </button>
                            )}
                          </div>
                        </article>
                      );
                    })}
                    {!payments.archived.length && (
                      <p className="text-xs text-stone-500">
                        Các đơn cũ sẽ tự lưu ở đây khi nhận khách tiếp theo.
                      </p>
                    )}
                    <div className="flex items-center justify-between gap-2 pt-1">
                      <button
                        type="button"
                        disabled={archivePage === 0}
                        className={`${buttonClass} text-xs py-1`}
                        onClick={() => setArchivePage(Math.max(0, archivePage - 1))}
                      >
                        Mới hơn
                      </button>
                      <span className="text-xs text-stone-500">Trang {archivePage + 1}</span>
                      <button
                        type="button"
                        disabled={(archivePage + 1) * COUNTER_ARCHIVE_PAGE_SIZE >= payments.archived.length}
                        className={`${buttonClass} text-xs py-1`}
                        onClick={() => setArchivePage(archivePage + 1)}
                      >
                        Cũ hơn
                      </button>
                    </div>
                  </section>
                )}

                {!payments.slots.length && (
                  <p className="text-xs text-stone-500 py-1">
                    Nhập số tiền phía trên và bấm Enter để tạo mã payOS riêng.
                  </p>
                )}
              </div>

              {/* Compact Recent Incoming Transactions */}
              <section
                aria-label="Giao dịch vừa nhận"
                className="rounded-2xl border border-stone-200/90 bg-white p-4 space-y-2.5 shadow-xs"
              >
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <span className="w-2 h-2 rounded-full bg-emerald-500 animate-pulse" />
                    <h3 className="font-bold text-stone-900 text-xs sm:text-sm">Giao dịch vừa nhận</h3>
                  </div>
                  <span className="text-[10px] font-semibold text-emerald-800 bg-emerald-50 border border-emerald-200 px-2 py-0.5 rounded-full">
                    Trực tiếp
                  </span>
                </div>

                {recent.data?.items && recent.data.items.length > 0 ? (
                  <div className="space-y-1.5">
                    {recent.data.items.slice(0, 4).map((tx) => (
                      <div
                        key={tx.id}
                        className="p-2 rounded-xl border border-stone-100 bg-stone-50/70 flex items-center justify-between gap-2 text-xs"
                      >
                        <div className="min-w-0">
                          <strong className="text-emerald-700 font-extrabold">
                            +{formatVndCurrency(Number(tx.credit))}
                          </strong>
                          <span className="text-stone-500 text-[11px] ml-2">
                            {tx.provider === 'SEPAY' ? 'SePay' : 'payOS'} · {tx.bank}
                          </span>
                          {tx.description && (
                            <p className="text-xs text-stone-600 mt-0.5 truncate">
                              {tx.description}
                            </p>
                          )}
                        </div>
                        <span className="text-[11px] text-stone-400">
                          {formatDateTimeVN(tx.transactionDate ?? tx.firstSeenAt)}
                        </span>
                      </div>
                    ))}
                  </div>
                ) : (
                  <p className="text-xs text-stone-500 py-2 text-center">Chưa có giao dịch tiền vào.</p>
                )}
              </section>
            </div>
          )}
        </div>
      </div>


      {/* Enlarged 2-Column Landscape Split Modal */}
      <EnlargedQRModal
        isOpen={enlarged}
        onClose={() => setEnlarged(false)}
        activeQr={activeQr}
        store={store.data}
        order={order}
        activeSlot={activeSlot}
        payload={payload}
        storeReady={storeReady}
        payable={payable}
        expired={expired}
        isTerminalOrder={!!order && isTerminalPaymentOrder(order.status)}
        latestCredit={latestCredit}
        recentTransactions={recent.data?.items ?? []}
        isRecentPending={recent.isPending}
        realtimeConnected={realtime.status === 'CONNECTED'}
      />

      {/* WiFi QR Modal */}
      <WifiQRModal isOpen={wifiOpen} onClose={() => setWifiOpen(false)} />
    </section>
  );
};
