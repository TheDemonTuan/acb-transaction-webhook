import React, { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  QrCode,
  X,
  Copy,
  Check,
  Building2,
  User,
  CreditCard,
  CheckCircle2,
  Sparkles,
  Clock,
  ShieldCheck,
  ArrowDownLeft,
  RefreshCw,
  Radio,
  Receipt,
} from 'lucide-react';
import { fetchPaymentQR, fetchTransactions } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { useRealtimeContext } from '../../realtime/RealtimeProvider';
import type { BankTransactionCreditData, RealtimeEnvelope } from '../../realtime/realtime.types';
import type { Transaction } from '../../realtime-types';
import { formatVndCurrency } from '../../shared/formatters/money';

interface LiveCreditAlert {
  id: string;
  transactionNumber: string;
  amount: number;
  description: string;
  timestamp: number; // Date.now()
  timeStr: string;
}

export const ReceivingQRModal: React.FC<{
  isOpen: boolean;
  onClose: () => void;
}> = ({ isOpen, onClose }) => {
  const queryClient = useQueryClient();
  const [copied, setCopied] = useState(false);
  const [activeTab, setActiveTab] = useState<'qr' | 'history'>('qr');
  const [sessionOpenedAt, setSessionOpenedAt] = useState<number>(Date.now());
  const [sessionCredits, setSessionCredits] = useState<LiveCreditAlert[]>([]);
  const [activeAlert, setActiveAlert] = useState<LiveCreditAlert | null>(null);
  const [nowTick, setNowTick] = useState<number>(Date.now());
  const seenTxIdsRef = useRef<Set<string>>(new Set());

  const { subscribe } = useRealtimeContext();

  const todayStr = useMemo(() => {
    return new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Ho_Chi_Minh' }).format(new Date());
  }, [isOpen]);

  // Load Payment QR settings
  const { data: qrData } = useQuery({
    queryKey: queryKeys.paymentQR,
    queryFn: fetchPaymentQR,
    enabled: isOpen,
  });

  // Load recent credit transactions for today
  const {
    data: txData,
    isLoading: loadingTx,
    refetch: refetchTx,
  } = useQuery({
    queryKey: queryKeys.transactions({ direction: 'credit', from: todayStr, to: todayStr, limit: 20 }),
    queryFn: () => fetchTransactions({ direction: 'credit', from: todayStr, to: todayStr, limit: 20 }),
    enabled: isOpen,
  });

  const qr = qrData?.qr;
  const isConfigured = qrData?.configured && qrData?.hasImage;

  // Reset state when modal opens
  useEffect(() => {
    if (isOpen) {
      const openTime = Date.now();
      setSessionOpenedAt(openTime);
      setSessionCredits([]);
      setActiveAlert(null);
      setActiveTab('qr');
      seenTxIdsRef.current.clear();
      refetchTx();
    }
  }, [isOpen, refetchTx]);

  // Second-by-second ticker for relative time displays ("vài giây trước")
  useEffect(() => {
    if (!isOpen) return;
    const interval = setInterval(() => setNowTick(Date.now()), 2000);
    return () => clearInterval(interval);
  }, [isOpen]);

  // Subscribe to live incoming payment events
  useEffect(() => {
    if (!isOpen) return;

    const unsub = subscribe<BankTransactionCreditData>(
      'bank.transaction.credit',
      (envelope: RealtimeEnvelope<BankTransactionCreditData>) => {
        const d = envelope.data;
        if (!d) return;

        // Strict source policy: ONLY REALTIME sources are live session credits
        if (d.source !== 'REALTIME') return;

        // Strict freshness policy: detected within last 120s and not before session opened
        const detectedAtMs = d.detectedAt ? new Date(d.detectedAt).getTime() : Date.now();
        if (Math.abs(Date.now() - detectedAtMs) > 120_000) return;
        if (detectedAtMs < sessionOpenedAt - 5_000) return;

        // Dedupe within this session
        if (seenTxIdsRef.current.has(d.transactionId)) return;
        seenTxIdsRef.current.add(d.transactionId);

        const creditVal = Number(d.credit || 0);
        if (creditVal > 0) {
          const now = Date.now();
          const alertItem: LiveCreditAlert = {
            id: d.transactionId,
            transactionNumber: d.transactionNumber,
            amount: creditVal,
            description: d.description || 'Chuyển khoản nhận tiền',
            timestamp: now,
            timeStr: new Date(now).toLocaleTimeString('vi-VN', {
              hour: '2-digit',
              minute: '2-digit',
              second: '2-digit',
            }),
          };

          setSessionCredits((prev) => [alertItem, ...prev]);
          setActiveAlert(alertItem);
          queryClient.invalidateQueries({ queryKey: queryKeys.transactions() });
        }
      }
    );

    return () => unsub();
  }, [isOpen, subscribe, queryClient, sessionOpenedAt]);

  // ESC key listener
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose();
      }
    };
    if (isOpen) {
      window.addEventListener('keydown', handleKeyDown);
    }
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, [isOpen, onClose]);

  if (!isOpen) return null;

  const handleCopy = (text: string) => {
    if (typeof navigator !== 'undefined') {
      navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  const formatRelativeTime = (timestamp: number) => {
    const diffSec = Math.max(0, Math.floor((nowTick - timestamp) / 1000));
    if (diffSec < 10) return 'Vừa nhận tức thì';
    if (diffSec < 60) return `${diffSec} giây trước`;
    const diffMin = Math.floor(diffSec / 60);
    if (diffMin < 60) return `${diffMin} phút trước`;
    return `${Math.floor(diffMin / 60)} giờ trước`;
  };

  const rawHistoryItems = txData?.items || [];

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-3 sm:p-4 md:p-6 bg-stone-950/70 backdrop-blur-xs animate-in fade-in duration-150">
      <div
        className="bg-white w-full max-w-md md:max-w-4xl lg:max-w-5xl rounded-3xl shadow-2xl border border-stone-200 overflow-hidden flex flex-col max-h-[94vh]"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Modal Topbar */}
        <div className="px-5 py-3.5 sm:px-6 sm:py-4 border-b border-stone-100 flex items-center justify-between shrink-0 bg-stone-50/70">
          <div className="flex items-center gap-3">
            <div className="w-9 h-9 rounded-2xl bg-emerald-600 text-white flex items-center justify-center shadow-xs">
              <QrCode className="w-5 h-5" />
            </div>
            <div>
              <div className="flex items-center gap-2">
                <h3 className="font-bold text-stone-900 text-sm sm:text-base leading-tight">
                  Quét mã nhận tiền ACB
                </h3>
                <span className="hidden sm:inline-flex items-center gap-1 px-2 py-0.5 rounded-md text-[11px] font-semibold bg-emerald-100 text-emerald-800 border border-emerald-200">
                  <ShieldCheck className="w-3 h-3 text-emerald-600" />
                  Theo dõi trực tiếp
                </span>
              </div>
              <p className="text-[11px] text-stone-500 flex items-center gap-1.5 mt-0.5">
                <span className="w-2 h-2 rounded-full bg-emerald-500 animate-pulse" />
                Đang trực tiếp theo dõi biến động số dư tài khoản
              </p>
            </div>
          </div>

          <button
            type="button"
            onClick={onClose}
            className="p-2 text-stone-400 hover:text-stone-700 rounded-xl hover:bg-stone-200/60 transition cursor-pointer"
            title="Đóng cửa sổ (ESC)"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        {/* Mobile Navigation Tabs (visible only on screens < md) */}
        <div className="flex md:hidden border-b border-stone-200 bg-stone-100/70 p-1 shrink-0">
          <button
            type="button"
            onClick={() => setActiveTab('qr')}
            className={`flex-1 py-2 text-xs font-bold rounded-xl transition cursor-pointer flex items-center justify-center gap-1.5 ${
              activeTab === 'qr'
                ? 'bg-white text-stone-900 shadow-xs'
                : 'text-stone-600 hover:text-stone-900'
            }`}
          >
            <QrCode className="w-3.5 h-3.5" />
            Mã QR nhận tiền
          </button>
          <button
            type="button"
            onClick={() => setActiveTab('history')}
            className={`flex-1 py-2 text-xs font-bold rounded-xl transition cursor-pointer flex items-center justify-center gap-1.5 relative ${
              activeTab === 'history'
                ? 'bg-white text-stone-900 shadow-xs'
                : 'text-stone-600 hover:text-stone-900'
            }`}
          >
            <Receipt className="w-3.5 h-3.5" />
            Lịch sử nhận tiền
            {sessionCredits.length > 0 && (
              <span className="w-2 h-2 rounded-full bg-emerald-500 animate-ping" />
            )}
          </button>
        </div>

        {/* Modal Body - 2 Columns on Desktop, Tabbed on Mobile */}
        <div className="flex-1 overflow-y-auto">
          <div className="grid grid-cols-1 md:grid-cols-12 min-h-full">
            {/* LEFT COLUMN: Large QR Code & Account Information (Col 5/12 on desktop) */}
            <div
              className={`md:col-span-5 p-5 sm:p-6 bg-stone-50/50 md:border-r border-stone-200/80 flex flex-col items-center justify-between text-center space-y-4 ${
                activeTab === 'qr' ? 'block' : 'hidden md:flex'
              }`}
            >
              {/* Mobile Realtime Alert Banner on QR screen */}
              {activeAlert && (
                <div className="w-full md:hidden bg-emerald-500 text-white rounded-2xl p-3.5 shadow-md border border-emerald-400 text-left animate-in slide-in-from-top-2 duration-200">
                  <div className="flex items-start justify-between gap-2">
                    <div className="flex items-start gap-2.5">
                      <CheckCircle2 className="w-5 h-5 text-white shrink-0 mt-0.5" />
                      <div>
                        <span className="text-[11px] font-bold uppercase tracking-wider text-emerald-100">
                          ĐÃ NHẬN TIỀN ({formatRelativeTime(activeAlert.timestamp)})
                        </span>
                        <p className="text-xl font-black text-white">
                          +{formatVndCurrency(activeAlert.amount)}
                        </p>
                        <p className="text-xs text-emerald-100 line-clamp-1">
                          {activeAlert.description}
                        </p>
                      </div>
                    </div>
                    <button
                      type="button"
                      onClick={() => setActiveAlert(null)}
                      className="text-white/80 hover:text-white p-1"
                    >
                      <X className="w-4 h-4" />
                    </button>
                  </div>
                </div>
              )}

              {isConfigured ? (
                <>
                  {/* Huge High-Contrast QR Code Card */}
                  <div className="w-full max-w-[290px] sm:max-w-[320px] bg-white p-4 rounded-3xl border-2 border-stone-200 shadow-md">
                    <div className="aspect-square bg-white flex items-center justify-center overflow-hidden rounded-2xl">
                      <img
                        src={qrData.imageURL}
                        alt="Mã QR nhận tiền ACB"
                        className="w-full h-full object-contain"
                      />
                    </div>
                  </div>

                  {/* Account Information with Copy Button */}
                  <div className="w-full max-w-[320px] bg-white rounded-2xl p-4 border border-stone-200 shadow-2xs text-xs text-left space-y-2.5">
                    <div className="flex items-center justify-between">
                      <span className="text-stone-500 flex items-center gap-1.5 font-medium">
                        <Building2 className="w-3.5 h-3.5 text-stone-400" />
                        Ngân hàng
                      </span>
                      <span className="font-bold text-stone-800">{qr.bankName} (Á Châu)</span>
                    </div>

                    <div className="flex items-center justify-between">
                      <span className="text-stone-500 flex items-center gap-1.5 font-medium">
                        <CreditCard className="w-3.5 h-3.5 text-stone-400" />
                        Số tài khoản
                      </span>
                      <div className="flex items-center gap-1.5">
                        <span className="font-mono font-black text-emerald-700 text-base">
                          {qr.accountNumber}
                        </span>
                        <button
                          type="button"
                          onClick={() => handleCopy(qr.accountNumber)}
                          className="p-1 text-stone-400 hover:text-stone-900 cursor-pointer hover:bg-stone-100 rounded-md transition"
                          title="Sao chép số tài khoản"
                        >
                          {copied ? (
                            <Check className="w-4 h-4 text-emerald-600" />
                          ) : (
                            <Copy className="w-4 h-4" />
                          )}
                        </button>
                      </div>
                    </div>

                    <div className="flex items-center justify-between">
                      <span className="text-stone-500 flex items-center gap-1.5 font-medium">
                        <User className="w-3.5 h-3.5 text-stone-400" />
                        Chủ tài khoản
                      </span>
                      <span className="font-black text-stone-900 uppercase">
                        {qr.accountName}
                      </span>
                    </div>
                  </div>

                  <p className="text-[11px] text-stone-500 leading-relaxed max-w-xs">
                    Quét bằng ứng dụng ngân hàng bất kỳ qua VietQR. Tiền vào được ghi nhận và hiển thị ngay trên màn hình này.
                  </p>
                </>
              ) : (
                <div className="py-16 space-y-3 text-stone-400">
                  <QrCode className="w-16 h-16 mx-auto stroke-1" />
                  <p className="text-sm font-bold text-stone-700">Chưa thiết lập mã QR nhận tiền</p>
                  <p className="text-xs text-stone-500 max-w-xs mx-auto">
                    Vào phần Quản trị &rarr; Kết nối ACB để tải ảnh QR hoặc tạo VietQR tự động.
                  </p>
                </div>
              )}
            </div>

            {/* RIGHT COLUMN: Live Monitor Status & Recent Transactions History (Col 7/12 on desktop) */}
            <div
              className={`md:col-span-7 p-5 sm:p-6 flex flex-col justify-between space-y-4 ${
                activeTab === 'history' ? 'block' : 'hidden md:flex'
              }`}
            >
              {/* TOP: Live Status & Celebration Banner */}
              <div className="space-y-3 shrink-0">
                {/* Active Session Arrival Banner */}
                {activeAlert ? (
                  <div className="bg-emerald-500 text-white rounded-2xl p-4 sm:p-5 shadow-lg border-2 border-emerald-400 text-left animate-in slide-in-from-top-3 fade-in duration-200 relative overflow-hidden">
                    <div className="absolute -right-6 -bottom-6 w-36 h-36 bg-white/10 rounded-full blur-2xl pointer-events-none" />
                    <div className="flex items-start justify-between gap-3 relative z-10">
                      <div className="flex items-start gap-3.5">
                        <div className="w-12 h-12 rounded-2xl bg-white/20 backdrop-blur-xs flex items-center justify-center shrink-0 shadow-xs">
                          <CheckCircle2 className="w-7 h-7 text-white animate-bounce" />
                        </div>
                        <div>
                          <div className="flex flex-wrap items-center gap-2">
                            <span className="inline-flex items-center gap-1 px-2.5 py-0.5 rounded-full text-[11px] font-black bg-white text-emerald-800 uppercase tracking-wide">
                              <Sparkles className="w-3 h-3 text-amber-500 fill-amber-500" />
                              VỪA NHẬN TIỀN THÀNH CÔNG
                            </span>
                            <span className="text-xs text-emerald-100 font-semibold">
                              ({formatRelativeTime(activeAlert.timestamp)})
                            </span>
                          </div>

                          <p className="text-3xl font-black tracking-tight text-white mt-1">
                            +{formatVndCurrency(activeAlert.amount)}
                          </p>

                          <div className="mt-2 space-y-1 text-xs text-emerald-50">
                            <p className="font-medium bg-emerald-600/60 px-2.5 py-1 rounded-lg">
                              Nội dung: <strong>{activeAlert.description}</strong>
                            </p>
                            <p className="text-[11px] text-emerald-100 font-mono">
                              Mã giao dịch: <strong>#{activeAlert.transactionNumber}</strong> • Lúc {activeAlert.timeStr}
                            </p>
                          </div>
                        </div>
                      </div>

                      <button
                        type="button"
                        onClick={() => setActiveAlert(null)}
                        className="p-1.5 text-white/70 hover:text-white rounded-lg hover:bg-white/10 transition cursor-pointer"
                        title="Ẩn thông báo"
                      >
                        <X className="w-4 h-4" />
                      </button>
                    </div>
                  </div>
                ) : (
                  /* Waiting for Transfer State */
                  <div className="bg-stone-50 border border-stone-200 rounded-2xl p-4 flex items-center justify-between gap-3">
                    <div className="flex items-center gap-3">
                      <div className="w-10 h-10 rounded-xl bg-emerald-50 text-emerald-600 border border-emerald-100 flex items-center justify-center shrink-0">
                        <Radio className="w-5 h-5 animate-pulse" />
                      </div>
                      <div>
                        <p className="text-xs font-bold text-stone-900 flex items-center gap-1.5">
                          Đang chờ khách hàng chuyển khoản...
                        </p>
                        <p className="text-[11px] text-stone-500 mt-0.5">
                          Phiên mở lúc {new Date(sessionOpenedAt).toLocaleTimeString('vi-VN')} • Tự động báo ngay khi tài khoản có tiền
                        </p>
                      </div>
                    </div>

                    <button
                      type="button"
                      onClick={() => refetchTx()}
                      disabled={loadingTx}
                      className="p-2 text-stone-500 hover:text-stone-900 rounded-xl hover:bg-stone-200/50 transition cursor-pointer"
                      title="Kiểm tra lại giao dịch"
                    >
                      <RefreshCw className={`w-4 h-4 ${loadingTx ? 'animate-spin' : ''}`} />
                    </button>
                  </div>
                )}
              </div>

              {/* BOTTOM: Anti-Fraud Recent Transactions Feed */}
              <div className="flex-1 flex flex-col min-h-0 space-y-2">
                <div className="flex items-center justify-between text-xs pb-1 border-b border-stone-100">
                  <div className="flex items-center gap-1.5">
                    <Clock className="w-3.5 h-3.5 text-stone-400" />
                    <span className="font-bold text-stone-800">Lịch sử nhận tiền gần nhất hôm nay</span>
                  </div>
                  <span className="text-[11px] text-stone-400">
                    Phân biệt rõ giao dịch mới vs cũ
                  </span>
                </div>

                <div className="flex-1 overflow-y-auto space-y-2 pr-1 max-h-[280px] sm:max-h-[320px]">
                  {loadingTx ? (
                    <div className="py-12 text-center text-xs text-stone-400">
                      <RefreshCw className="w-5 h-5 animate-spin mx-auto mb-2 text-stone-300" />
                      Đang tải danh sách giao dịch...
                    </div>
                  ) : rawHistoryItems.length === 0 && sessionCredits.length === 0 ? (
                    <div className="py-12 text-center text-xs text-stone-400 space-y-1">
                      <Receipt className="w-8 h-8 mx-auto text-stone-300" />
                      <p className="font-medium text-stone-600">Chưa có giao dịch nhận tiền nào hôm nay</p>
                    </div>
                  ) : (
                    <>
                      {/* 1. Transactions received in this active modal session */}
                      {sessionCredits.map((item) => (
                        <div
                          key={`session-${item.id}`}
                          className="p-3 rounded-2xl bg-emerald-50/80 border-2 border-emerald-400 shadow-2xs flex items-center justify-between gap-3 text-xs"
                        >
                          <div className="flex items-center gap-2.5 min-w-0">
                            <div className="w-8 h-8 rounded-xl bg-emerald-600 text-white flex items-center justify-center shrink-0">
                              <ArrowDownLeft className="w-4 h-4" />
                            </div>
                            <div className="min-w-0">
                              <div className="flex items-center gap-1.5">
                                <span className="px-2 py-0.5 rounded-md font-black text-[10px] bg-emerald-600 text-white animate-pulse">
                                  VỪA NHẬN TRONG PHIÊN NÀY
                                </span>
                                <span className="text-[11px] font-semibold text-emerald-800">
                                  {item.timeStr}
                                </span>
                              </div>
                              <p className="font-medium text-stone-900 truncate max-w-xs mt-0.5">
                                {item.description}
                              </p>
                              <span className="text-[10px] font-mono text-stone-500">
                                Mã GD: #{item.transactionNumber}
                              </span>
                            </div>
                          </div>
                          <div className="text-right shrink-0">
                            <span className="font-black text-sm text-emerald-700 block">
                              +{formatVndCurrency(item.amount)}
                            </span>
                            <span className="text-[10px] text-emerald-600 font-semibold">
                              {formatRelativeTime(item.timestamp)}
                            </span>
                          </div>
                        </div>
                      ))}

                      {/* 2. Earlier transactions (received before opening this modal) */}
                      {rawHistoryItems
                        .filter((tx) => !sessionCredits.some((sc) => sc.id === tx.id))
                        .map((tx) => (
                          <div
                            key={tx.id}
                            className="p-3 rounded-2xl bg-stone-50/80 border border-stone-200/80 hover:bg-stone-100/70 transition flex items-center justify-between gap-3 text-xs"
                          >
                            <div className="flex items-center gap-2.5 min-w-0">
                              <div className="w-8 h-8 rounded-xl bg-stone-200 text-stone-600 flex items-center justify-center shrink-0">
                                <ArrowDownLeft className="w-4 h-4" />
                              </div>
                              <div className="min-w-0">
                                <div className="flex items-center gap-1.5">
                                  <span className="px-1.5 py-0.5 rounded text-[10px] font-medium bg-stone-200 text-stone-600">
                                    ĐÃ NHẬN TRƯỚC ĐÓ
                                  </span>
                                  <span className="text-[11px] text-stone-500 font-medium">
                                    {tx.transactionDate || tx.firstSeenAt}
                                  </span>
                                </div>
                                <p className="font-medium text-stone-800 truncate max-w-xs mt-0.5">
                                  {tx.description || 'Không có nội dung'}
                                </p>
                                <span className="text-[10px] font-mono text-stone-400">
                                  {tx.semanticKey}
                                </span>
                              </div>
                            </div>
                            <div className="text-right shrink-0">
                              <span className="font-bold text-sm text-stone-700 block">
                                +{formatVndCurrency(tx.credit)}
                              </span>
                              {tx.balance && (
                                <span className="text-[10px] text-stone-400 font-mono">
                                  Dư: {formatVndCurrency(tx.balance)}
                                </span>
                              )}
                            </div>
                          </div>
                        ))}
                    </>
                  )}
                </div>
              </div>

              {/* Bottom Note */}
              <div className="pt-2 border-t border-stone-100 flex items-center justify-between text-[11px] text-stone-500">
                <span>
                  Được bảo vệ bởi <strong>Hệ thống Gateway ACB</strong>
                </span>
                <button
                  type="button"
                  onClick={() => onClose()}
                  className="text-stone-700 hover:text-stone-900 font-semibold cursor-pointer"
                >
                  Xong &rarr;
                </button>
              </div>
            </div>
          </div>
        </div>

        {/* Modal Footer */}
        <div className="px-6 py-3.5 bg-stone-50 border-t border-stone-100 flex items-center justify-between shrink-0">
          <span className="text-xs text-stone-500">
            {sessionCredits.length > 0 ? (
              <span className="text-emerald-700 font-semibold flex items-center gap-1">
                <CheckCircle2 className="w-3.5 h-3.5 text-emerald-600" />
                Đã ghi nhận {sessionCredits.length} giao dịch trong phiên này
              </span>
            ) : (
              'Bấm Đóng hoặc phím ESC khi hoàn tất'
            )}
          </span>
          <button
            type="button"
            onClick={onClose}
            className="px-5 py-2 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 transition shadow-xs cursor-pointer"
          >
            Đóng
          </button>
        </div>
      </div>
    </div>
  );
};
