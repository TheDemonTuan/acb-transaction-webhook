import React, { useEffect, useRef, useState } from 'react';
import {
  CheckCircle2,
  Copy,
  ExternalLink,
  Sparkles,
  Wifi,
  X,
  Radio,
  ArrowDownLeft,
} from 'lucide-react';
import { PaymentQRImage } from './PaymentQRImage';
import { formatVndCurrency } from '../../shared/formatters/money';
import { formatDateTimeVN } from '../../shared/formatters/datetime';
import type { PaymentOrder, PaymentOrderSlot } from './payment-orders';
import type { Transaction } from '../../realtime-types';
import type { SePayStoreConfig } from './sepay-store';

export interface RecentCreditAlert {
  id: string;
  amountVnd: number;
  bank?: string;
  provider?: string;
  orderCode?: string;
  time: string;
  reference?: string;
  timestamp: number;
}

export interface EnlargedQRModalProps {
  isOpen: boolean;
  onClose: () => void;
  activeQr: { kind: 'store' } | { kind: 'payos'; slotId: string };
  store?: SePayStoreConfig;
  order?: PaymentOrder;
  activeSlot?: PaymentOrderSlot;
  payload: string;
  storeReady: boolean;
  payable: boolean;
  expired: boolean;
  isTerminalOrder: boolean;
  latestCredit: RecentCreditAlert | null;
  recentTransactions: Transaction[];
  isRecentPending?: boolean;
  realtimeConnected: boolean;
}

export const EnlargedQRModal: React.FC<EnlargedQRModalProps> = ({
  isOpen,
  onClose,
  activeQr,
  store,
  order,
  activeSlot,
  payload,
  storeReady,
  payable,
  expired,
  isTerminalOrder,
  latestCredit,
  recentTransactions,
  isRecentPending,
  realtimeConnected,
}) => {
  const dialogRef = useRef<HTMLDivElement>(null);
  const [copyNotice, setCopyNotice] = useState('');

  useEffect(() => {
    if (!isOpen) return;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    dialogRef.current?.focus();
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        onClose();
      }
      if (event.key !== 'Tab') return;
      const nodes = dialogRef.current?.querySelectorAll<HTMLElement>(
        'button:not([disabled]), a[href], input:not([disabled]), [tabindex="0"]'
      );
      if (!nodes?.length) {
        event.preventDefault();
        return;
      }
      const first = nodes[0];
      const last = nodes[nodes.length - 1];
      if (event.shiftKey && (document.activeElement === first || document.activeElement === dialogRef.current)) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || document.activeElement === dialogRef.current)) {
        event.preventDefault();
        first.focus();
      }
    };

    const keepFocus = (event: FocusEvent) => {
      if (event.target instanceof Node && !dialogRef.current?.contains(event.target)) {
        dialogRef.current?.focus();
      }
    };
    document.addEventListener('keydown', handleKeyDown);
    document.addEventListener('focusin', keepFocus);
    return () => {
      document.body.style.overflow = previousOverflow;
      document.removeEventListener('keydown', handleKeyDown);
      document.removeEventListener('focusin', keepFocus);
      opener?.focus({ preventScroll: true });
    };
  }, [isOpen, onClose]);

  if (!isOpen) return null;

  const handleCopyInfo = () => {
    let text = '';
    if (activeQr.kind === 'store' && store) {
      text = `${store.bank} · ${store.accountName}\nSTK: ${store.accountNumber}`;
    } else if (order) {
      text = `${order.accountNumber ?? ''}\n${order.amountVnd} VND\nDH${order.orderCode}`;
    }
    if (!text) return;
    void navigator.clipboard.writeText(text).then(
      () => {
        setCopyNotice('Đã sao chép thông tin nhận tiền');
        setTimeout(() => setCopyNotice(''), 2500);
      },
      () => setCopyNotice('Không thể sao chép.')
    );
  };

  const isStore = activeQr.kind === 'store';
  const title = isStore
    ? store?.storeName || 'QR Cửa hàng · SePay'
    : `payOS · ${activeSlot?.name || 'Đơn thanh toán'}`;
  const amountDisplay = isStore
    ? 'Khách tự nhập số tiền'
    : activeSlot
    ? formatVndCurrency(activeSlot.amountVnd)
    : '';

  return (
    <div className="fixed inset-0 z-50 bg-stone-950/80 backdrop-blur-xs flex items-center justify-center p-3 sm:p-5 animate-in fade-in">
      <div
        ref={dialogRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label="Phóng to QR"
        className="w-full max-w-4xl lg:max-w-5xl max-h-[92vh] md:h-[650px] rounded-3xl bg-white shadow-2xl flex flex-col overflow-hidden border border-stone-200 outline-none"
      >
        {/* Top Header Bar */}
        <div className="flex items-center justify-between px-5 py-3.5 border-b border-stone-200 bg-stone-50/80 shrink-0">
          <div className="flex items-center gap-2.5 min-w-0">
            <span
              className={`w-2.5 h-2.5 rounded-full ${
                realtimeConnected ? 'bg-emerald-500 animate-pulse' : 'bg-amber-500'
              }`}
            />
            <h2 className="font-bold text-stone-900 text-sm sm:text-base truncate">{title}</h2>
            <span
              className={`text-[11px] font-semibold px-2 py-0.5 rounded-full hidden sm:inline-flex ${
                isStore
                  ? 'bg-emerald-100 text-emerald-800 border border-emerald-200'
                  : 'bg-blue-100 text-blue-800 border border-blue-200'
              }`}
            >
              {isStore ? 'SePay Store' : 'payOS Đơn lẻ'}
            </span>
          </div>

          <div className="flex items-center gap-2">
            <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-[11px] font-semibold bg-emerald-50 text-emerald-800 border border-emerald-200">
              <Radio className="w-3 h-3 text-emerald-600 animate-pulse" />
              <span>Realtime Live</span>
            </span>
            <button
              type="button"
              onClick={onClose}
              aria-label="Đóng QR phóng to"
              className="w-8 h-8 flex items-center justify-center rounded-xl text-stone-400 hover:text-stone-800 hover:bg-stone-200/60 transition cursor-pointer"
            >
              <X className="w-5 h-5" />
            </button>
          </div>
        </div>

        {/* 2-Column Split Content */}
        <div className="flex-1 grid grid-cols-1 md:grid-cols-2 divide-y md:divide-y-0 md:divide-x divide-stone-200 min-h-0 overflow-y-auto md:overflow-hidden">
          {/* Cột trái: QR Code to rõ, thông tin nhận tiền */}
          <div className="p-5 sm:p-6 flex flex-col items-center justify-center text-center bg-white overflow-y-auto min-h-0">
            <div className="w-full max-w-sm flex flex-col items-center gap-3">
              <div className="text-center">
                <span className="text-xs font-bold uppercase tracking-wider text-emerald-700">
                  {isStore ? 'QR chuyển khoản cửa hàng' : 'Mã QR thanh toán đơn hàng'}
                </span>
                <p className="text-2xl font-black text-stone-900 tracking-tight mt-0.5">
                  {amountDisplay}
                </p>
              </div>

              {/* QR Image rendering */}
              <div className="bg-stone-50 p-3.5 rounded-2xl border border-stone-200 shadow-2xs w-full flex flex-col items-center [&_img]:w-[240px] sm:[&_img]:w-[260px] md:[&_img]:w-[280px]">
                {isStore ? (
                  storeReady ? (
                    <>
                      <PaymentQRImage payload={payload} alt="QR chuyển khoản cửa hàng SePay" />
                      <div className="mt-2 text-center space-y-0.5">
                        <p className="font-bold text-stone-900 text-sm sm:text-base">{store!.storeName}</p>
                        <p className="text-xs text-stone-600">
                          {store!.bank} · <span className="font-semibold">{store!.accountName}</span>
                        </p>
                        <p className="font-mono text-sm sm:text-base font-bold text-stone-800 select-all tracking-wider">
                          {store!.accountNumber}
                        </p>
                      </div>
                    </>
                  ) : (
                    <div className="py-12 text-center text-stone-500 text-xs">
                      QR cửa hàng chưa sẵn sàng
                    </div>
                  )
                ) : order?.status === 'PAID' ? (
                  <div className="w-full py-8 text-center text-emerald-900 space-y-2">
                    <div className="w-12 h-12 bg-emerald-100 rounded-full flex items-center justify-center mx-auto text-emerald-600">
                      <CheckCircle2 className="w-7 h-7" />
                    </div>
                    <h3 className="text-xl font-bold">Thanh toán thành công</h3>
                    <p className="text-sm font-semibold">Đã nhận {formatVndCurrency(order.amountVnd)}</p>
                    <p className="text-xs text-stone-500">Mã đơn: {order.orderCode}</p>
                  </div>
                ) : order && (expired || isTerminalOrder) ? (
                  <div className="py-12 text-center text-amber-900 text-xs px-4">
                    {expired && !isTerminalOrder
                      ? 'Đã hết thời gian hiển thị QR.'
                      : 'Đơn hàng không còn hiệu lực.'}
                  </div>
                ) : payable && payload ? (
                  <>
                    <PaymentQRImage payload={payload} alt={`QR thanh toán ${activeSlot?.name}`} />
                    <div className="mt-2 text-center space-y-0.5">
                      <p className="font-bold text-stone-900 text-sm">
                        KienlongBank · {order!.accountName}
                      </p>
                      <p className="font-mono text-sm font-bold text-stone-800 select-all">
                        {order!.accountNumber}
                      </p>
                      <p className="text-xs text-stone-500">Mã đơn: {order!.orderCode}</p>
                      <p className="text-[11px] text-stone-400">
                        Hạn: {formatDateTimeVN(order!.expiresAt)}
                      </p>
                    </div>
                  </>
                ) : (
                  <div className="py-12 text-center text-stone-500 text-xs">
                    Đang chuẩn bị mã QR...
                  </div>
                )}
              </div>

              <p className="text-xs text-stone-500">
                Khách dùng ứng dụng ngân hàng hoặc ví điện tử bất kỳ để quét
              </p>
            </div>
          </div>

          {/* Cột phải: Live Incoming Transactions & Instant Celebration Alert */}
          <div className="p-5 sm:p-6 flex flex-col bg-stone-50/60 min-h-0 overflow-hidden">
            <div className="flex items-center justify-between pb-3 border-b border-stone-200/80 shrink-0">
              <div className="flex items-center gap-2">
                <span className="w-2.5 h-2.5 rounded-full bg-emerald-500 animate-pulse" />
                <h3 className="font-bold text-stone-900 text-sm sm:text-base">
                  Đối soát tiền vào trực tiếp
                </h3>
              </div>
              <span className="text-[10px] font-bold text-emerald-800 bg-emerald-100 border border-emerald-200 px-2 py-0.5 rounded-full">
                Tự động nhảy số
              </span>
            </div>

            {/* Instant Celebration Alert for the latest transaction */}
            {latestCredit && Date.now() - latestCredit.timestamp < 180_000 && (
              <div className="my-3 rounded-2xl bg-gradient-to-r from-emerald-600 to-emerald-700 text-white p-3.5 shadow-md text-left animate-in fade-in zoom-in-95 border border-emerald-400/50 shrink-0">
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-1.5 font-bold text-xs uppercase tracking-wider text-emerald-100">
                    <Sparkles className="w-3.5 h-3.5 text-yellow-300" />
                    <span>VỪA NHẬN TIỀN THÀNH CÔNG!</span>
                  </div>
                  <span className="text-[10px] bg-white/20 px-2 py-0.5 rounded-full font-medium">
                    {latestCredit.time}
                  </span>
                </div>
                <p className="text-2xl font-black mt-0.5 tracking-tight">
                  +{formatVndCurrency(latestCredit.amountVnd)}
                </p>
                <p className="text-xs text-emerald-100 truncate mt-0.5">
                  {latestCredit.provider === 'SEPAY' ? 'SePay Store' : 'payOS'} ·{' '}
                  {latestCredit.bank || 'Ngân hàng'} · Mã: {latestCredit.reference || latestCredit.orderCode || latestCredit.id}
                </p>
              </div>
            )}

            {/* Scrollable Live Transactions Feed */}
            <div className="flex-1 overflow-y-auto space-y-2 pr-1 min-h-0 mt-2">
              <div className="flex items-center justify-between text-xs font-semibold text-stone-600 mb-1">
                <span>Giao dịch tiền vào gần nhất:</span>
                <span>{recentTransactions.length} giao dịch</span>
              </div>

              {recentTransactions.length > 0 ? (
                recentTransactions.slice(0, 8).map((tx) => {
                  const isVeryRecent =
                    Date.now() - Date.parse(tx.firstSeenAt || tx.transactionDate || '') < 180_000;
                  return (
                    <div
                      key={tx.id}
                      className={`p-3 rounded-xl border transition flex items-center justify-between gap-3 text-left ${
                        isVeryRecent
                          ? 'border-emerald-300 bg-white shadow-xs ring-1 ring-emerald-400'
                          : 'border-stone-200/70 bg-white hover:bg-stone-50'
                      }`}
                    >
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-1.5">
                          <strong className="text-emerald-700 font-extrabold text-base">
                            +{formatVndCurrency(Number(tx.credit))}
                          </strong>
                          <span
                            className={`text-[10px] font-bold px-1.5 py-0.5 rounded-md ${
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
                            <span className="text-[9px] font-bold text-white bg-emerald-600 px-1.5 py-0.5 rounded-full animate-pulse">
                              VỪA NHẬN
                            </span>
                          )}
                        </div>
                        <p className="text-xs text-stone-500 mt-1 truncate">
                          {tx.bank ? `${tx.bank} · ` : ''}
                          {formatDateTimeVN(tx.transactionDate ?? tx.firstSeenAt)}
                          {tx.description && tx.description !== 'Thanh toán QR cửa hàng'
                            ? ` · ${tx.description}`
                            : ''}
                        </p>
                      </div>
                    </div>
                  );
                })
              ) : (
                <div className="h-full min-h-[160px] flex flex-col items-center justify-center text-center p-6 text-stone-500 text-xs">
                  <ArrowDownLeft className="w-8 h-8 text-stone-300 mb-2" />
                  <p className="font-semibold text-stone-700">Chưa có giao dịch mới</p>
                  <p className="text-stone-400 mt-0.5">
                    {isRecentPending
                      ? 'Đang tải danh sách giao dịch...'
                      : 'Hệ thống đang sẵn sàng ghi nhận tiền chuyển vào tức thì.'}
                  </p>
                </div>
              )}
            </div>
          </div>
        </div>

        {/* Modal Bottom Actions */}
        <div className="px-5 py-3 border-t border-stone-200 bg-white flex flex-wrap items-center justify-between gap-3 shrink-0">
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={handleCopyInfo}
              className="min-h-10 inline-flex items-center gap-1.5 px-3.5 py-1.5 rounded-xl border border-stone-300 text-xs font-semibold text-stone-700 hover:bg-stone-50 transition cursor-pointer"
            >
              <Copy className="w-3.5 h-3.5" />
              <span>Sao chép thông tin</span>
            </button>
            {copyNotice && (
              <span role="status" className="text-xs font-semibold text-emerald-700 animate-in fade-in">
                {copyNotice}
              </span>
            )}
          </div>

          <button
            type="button"
            onClick={onClose}
            className="min-h-10 px-5 py-1.5 rounded-xl bg-stone-900 text-white text-xs font-bold hover:bg-stone-800 transition cursor-pointer"
          >
            Đóng QR phóng to
          </button>
        </div>
      </div>
    </div>
  );
};
