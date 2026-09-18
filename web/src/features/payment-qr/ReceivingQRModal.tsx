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
  Smartphone,
} from 'lucide-react';
import { fetchPaymentQR, fetchTransactions } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { useRealtimeContext } from '../../realtime/RealtimeProvider';
import type { BankTransactionCreditData, PaymentActivationData, RealtimeEnvelope } from '../../realtime/realtime.types';
import type { Transaction } from '../../realtime-types';
import { formatVndCurrency } from '../../shared/formatters/money';
import {
  parseCreditAmount,
  shouldAcceptLiveCredit,
} from './credit-filter';
import {
  ACTIVATION_IDENTIFIER_TTL_MS,
  activationURL,
  newActivationIdentifier,
} from './activation-qr';
import {
  isQRReadyToDisplay,
  selectQRImageURL,
} from './qr-payload';
import QRCode from 'qrcode';

interface LiveCreditAlert {
  id: string;
  transactionNumber: string;
  amount: number;
  description: string;
  timestamp: number; // Date.now()
  timeStr: string;
}

interface CustomerScanAlert {
  identifier: string;
  timestamp: number;
  timeStr: string;
  phase?: string;
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
  const [customerScanned, setCustomerScanned] = useState<CustomerScanAlert | null>(null);
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

  const [activationId, setActivationId] = useState<string>(() => newActivationIdentifier());
  const [activationExpiresAt, setActivationExpiresAt] = useState<number>(() => Date.now() + ACTIVATION_IDENTIFIER_TTL_MS);
  const [copiedLink, setCopiedLink] = useState(false);
  const [showDirectVietQR, setShowDirectVietQR] = useState(false);
  const activationCanvasRef = useRef<HTMLCanvasElement | null>(null);

  const qr = qrData?.qr;
  const qrImageURL = selectQRImageURL(qrData);
  const isConfigured = isQRReadyToDisplay(qrData);

  // Reset state and rotate activation identifier when modal opens
  useEffect(() => {
    if (isOpen) {
      const openTime = Date.now();
      setSessionOpenedAt(openTime);
      setSessionCredits([]);
      setActiveAlert(null);
      setCustomerScanned(null);
      setActiveTab('qr');
      setShowDirectVietQR(false);
      seenTxIdsRef.current.clear();
      setActivationId(newActivationIdentifier());
      setActivationExpiresAt(openTime + ACTIVATION_IDENTIFIER_TTL_MS);
      refetchTx();
    }
  }, [isOpen, refetchTx]);

  // Periodic rotation of activation identifier every TTL while modal remains open
  useEffect(() => {
    if (!isOpen) return;
    const interval = setInterval(() => {
      setActivationId(newActivationIdentifier());
      setActivationExpiresAt(Date.now() + ACTIVATION_IDENTIFIER_TTL_MS);
    }, ACTIVATION_IDENTIFIER_TTL_MS);
    return () => clearInterval(interval);
  }, [isOpen]);

  // Render activation QR to canvas whenever activationId changes or modal opens
  useEffect(() => {
    if (!isOpen || !activationCanvasRef.current) return;
    const url = activationURL(activationId);
    QRCode.toCanvas(activationCanvasRef.current, url, {
      width: 240,
      margin: 1,
      color: {
        dark: '#0f172a',
        light: '#ffffff',
      },
      errorCorrectionLevel: 'M',
    }).catch((err) => {
      console.warn('failed to render activation QR canvas:', err);
    });
  }, [isOpen, activationId, activeTab, showDirectVietQR]);

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

        const accepted = shouldAcceptLiveCredit(d, {
          sessionOpenedAt,
          seenIds: seenTxIdsRef.current,
          sources: ['REALTIME'],
          now: Date.now(),
        });
        if (!accepted) return;

        const creditVal = parseCreditAmount(d.credit);
        if (creditVal > 0) {
          const now = Date.now();
          const txId = d.transactionId || d.transactionNumber || String(now);
          const alertItem: LiveCreditAlert = {
            id: txId,
            transactionNumber: d.transactionNumber || txId,
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

    const unsubActivation = subscribe<PaymentActivationData>(
      'payment.activated',
      (envelope: RealtimeEnvelope<PaymentActivationData>) => {
        const d = envelope.data;
        if (!d) return;

        const now = Date.now();
        setCustomerScanned({
          identifier: d.identifier,
          timestamp: now,
          timeStr: new Date(now).toLocaleTimeString('vi-VN', {
            hour: '2-digit',
            minute: '2-digit',
            second: '2-digit',
          }),
          phase: d.phase || 'HOT',
        });
      }
    );

    return () => {
      unsub();
      unsubActivation();
    };
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
                {customerScanned && !activeAlert ? (
                  <>
                    <span className="w-2 h-2 rounded-full bg-indigo-500 animate-ping" />
                    <span className="font-semibold text-indigo-700">
                      Khách vừa quét mã ({formatRelativeTime(customerScanned.timestamp)})
                    </span>
                    <span className="text-stone-400">• Đang chờ tiền vào</span>
                  </>
                ) : (
                  <>
                    <span className="w-2 h-2 rounded-full bg-emerald-500 animate-pulse" />
                    <span>Đang trực tiếp theo dõi biến động số dư tài khoản</span>
                  </>
                )}
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
            {customerScanned && !activeAlert && (
              <span className="w-2 h-2 rounded-full bg-indigo-500 animate-ping" />
            )}
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
              {activeAlert ? (
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
              ) : customerScanned ? (
                <div className="w-full md:hidden bg-gradient-to-r from-indigo-600 to-blue-600 text-white rounded-2xl p-3.5 shadow-md border border-indigo-400 text-left animate-in slide-in-from-top-2 duration-200">
                  <div className="flex items-start justify-between gap-2">
                    <div className="flex items-start gap-2.5">
                      <Smartphone className="w-5 h-5 text-white shrink-0 mt-0.5 animate-pulse" />
                      <div>
                        <div className="flex items-center gap-1.5">
                          <span className="text-[10px] font-black uppercase tracking-wider bg-white/20 px-1.5 py-0.5 rounded text-white">
                            ĐÃ QUÉT MÃ
                          </span>
                          <span className="text-[11px] text-blue-100 font-mono">
                            {formatRelativeTime(customerScanned.timestamp)}
                          </span>
                        </div>
                        <p className="text-xs font-bold text-white mt-1">
                          Khách đang mở ACB ONE / trang thanh toán
                        </p>
                        <p className="text-[10px] text-blue-100 mt-0.5">
                          Hệ thống đã kích hoạt chế độ siêu tốc BURST...
                        </p>
                      </div>
                    </div>
                    <button
                      type="button"
                      onClick={() => setCustomerScanned(null)}
                      className="text-white/80 hover:text-white p-1"
                    >
                      <X className="w-4 h-4" />
                    </button>
                  </div>
                </div>
              ) : null}

              {isConfigured ? (
                <>
                  {/* Primary Card: Activation QR (Scan to Boost & Pay) or Direct VietQR */}
                  <div
                    className={`w-full max-w-[290px] sm:max-w-[320px] bg-white p-4 rounded-3xl border-2 transition-all duration-300 shadow-md ${
                      customerScanned && !showDirectVietQR && !activeAlert
                        ? 'border-indigo-500 ring-4 ring-indigo-500/20 shadow-indigo-100'
                        : 'border-stone-200'
                    }`}
                  >
                    <div className="aspect-square bg-white flex items-center justify-center overflow-hidden rounded-2xl relative">
                      {showDirectVietQR && qrImageURL ? (
                        <img
                          src={qrImageURL}
                          alt="Mã VietQR nhận tiền ACB"
                          className="w-full h-full object-contain"
                        />
                      ) : (
                        <>
                          <canvas
                            ref={activationCanvasRef}
                            className="w-full h-full object-contain"
                          />
                          {customerScanned && !activeAlert && (
                            <div className="absolute top-2 right-2 px-2 py-0.5 rounded-md bg-indigo-600 text-white text-[10px] font-bold shadow-md flex items-center gap-1 animate-pulse">
                              <span className="w-1.5 h-1.5 rounded-full bg-emerald-300 animate-ping" />
                              ĐÃ QUÉT
                            </div>
                          )}
                        </>
                      )}
                    </div>

                    <div className="mt-2 text-center">
                      {customerScanned && !showDirectVietQR && !activeAlert ? (
                        <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-[11px] font-bold bg-indigo-50 text-indigo-700 border border-indigo-200 animate-pulse">
                          <span className="w-2 h-2 rounded-full bg-indigo-500 animate-ping" />
                          Đã nhận diện thiết bị khách quét mã!
                        </span>
                      ) : (
                        <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[10px] font-semibold bg-indigo-50 text-indigo-700 border border-indigo-200">
                          <Sparkles className="w-3 h-3 text-indigo-600" />
                          {showDirectVietQR ? 'VietQR tĩnh' : 'Mã kích hoạt theo dõi tự động'}
                        </span>
                      )}
                    </div>
                  </div>

                  {/* Activation Link & Switcher Actions */}
                  <div className="w-full max-w-[320px] flex items-center justify-between gap-2 text-xs">
                    <button
                      type="button"
                      onClick={() => {
                        const url = activationURL(activationId);
                        navigator.clipboard.writeText(url);
                        setCopiedLink(true);
                        setTimeout(() => setCopiedLink(false), 2000);
                      }}
                      className="flex-1 inline-flex items-center justify-center gap-1.5 py-2 px-3 bg-stone-100 hover:bg-stone-200 text-stone-700 font-medium rounded-xl transition"
                    >
                      {copiedLink ? (
                        <>
                          <Check className="w-3.5 h-3.5 text-emerald-600" />
                          <span>Đã chép link</span>
                        </>
                      ) : (
                        <>
                          <Copy className="w-3.5 h-3.5 text-stone-500" />
                          <span>Sao chép link /pay</span>
                        </>
                      )}
                    </button>
                    <button
                      type="button"
                      onClick={() => setShowDirectVietQR((prev) => !prev)}
                      className="py-2 px-3 bg-white border border-stone-200 hover:bg-stone-50 text-stone-700 font-medium rounded-xl transition"
                    >
                      {showDirectVietQR ? 'Hiện QR kích hoạt' : 'Hiện VietQR gốc'}
                    </button>
                  </div>

                  {/* Account Information with Copy Button */}
                  <div className="w-full max-w-[320px] bg-white rounded-2xl p-4 border border-stone-200 shadow-2xs text-xs text-left space-y-2.5">
                    <div className="flex items-center justify-between">
                      <span className="text-stone-500 flex items-center gap-1.5 font-medium">
                        <Building2 className="w-3.5 h-3.5 text-stone-400" />
                        Ngân hàng
                      </span>
                      <span className="font-bold text-stone-800">{qr?.bankName || 'ACB'} (Á Châu)</span>
                    </div>

                    <div className="flex items-center justify-between">
                      <span className="text-stone-500 flex items-center gap-1.5 font-medium">
                        <CreditCard className="w-3.5 h-3.5 text-stone-400" />
                        Số tài khoản
                      </span>
                      <div className="flex items-center gap-1.5">
                        <span className="font-mono font-black text-emerald-700 text-base">
                          {qr?.accountNumber}
                        </span>
                        <button
                          type="button"
                          onClick={() => handleCopy(qr?.accountNumber || '')}
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
                        {qr?.accountName}
                      </span>
                    </div>
                  </div>

                  <p className="text-[11px] text-stone-500 leading-relaxed max-w-xs">
                    Khách quét mã kích hoạt bằng điện thoại để mở trang thanh toán và tự động tăng tốc nhận tiền trong 3 phút.
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
                ) : customerScanned ? (
                  /* Live Customer Scan Detection Banner */
                  <div className="bg-gradient-to-br from-indigo-600 via-blue-600 to-indigo-700 text-white rounded-2xl p-4 sm:p-5 shadow-lg border-2 border-indigo-400 text-left animate-in slide-in-from-top-3 fade-in duration-200 relative overflow-hidden">
                    <div className="absolute -right-6 -bottom-6 w-36 h-36 bg-white/10 rounded-full blur-2xl pointer-events-none" />
                    <div className="flex items-start justify-between gap-3 relative z-10">
                      <div className="flex items-start gap-3.5">
                        <div className="w-12 h-12 rounded-2xl bg-white/20 backdrop-blur-xs flex items-center justify-center shrink-0 shadow-xs ring-2 ring-white/30">
                          <Smartphone className="w-6 h-6 text-white animate-pulse" />
                        </div>
                        <div>
                          <div className="flex flex-wrap items-center gap-2">
                            <span className="inline-flex items-center gap-1 px-2.5 py-0.5 rounded-full text-[11px] font-black bg-white text-indigo-900 uppercase tracking-wide shadow-xs">
                              <Sparkles className="w-3 h-3 text-amber-500 fill-amber-500" />
                              ĐÃ PHÁT HIỆN KHÁCH QUÉT MÃ
                            </span>
                            <span className="text-xs text-indigo-100 font-semibold font-mono">
                              ({formatRelativeTime(customerScanned.timestamp)})
                            </span>
                          </div>

                          <p className="text-base sm:text-lg font-black tracking-tight text-white mt-1.5">
                            Khách đang mở ACB ONE / trang thanh toán
                          </p>

                          <p className="text-xs text-indigo-100/90 leading-relaxed mt-1">
                            Hệ thống đã nhận diện thiết bị và tự động kích hoạt chế độ siêu tốc <strong className="text-white font-bold">BURST ({customerScanned.phase || 'HOT'})</strong>. Số dư sẽ tự động cập nhật ngay khi khách xác nhận chuyển tiền.
                          </p>

                          <div className="mt-3 flex flex-wrap items-center gap-2 text-[11px]">
                            <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-lg bg-white/15 text-white font-medium backdrop-blur-xs">
                              <span className="w-2 h-2 rounded-full bg-emerald-400 animate-ping" />
                              Polling siêu tốc: 1.5s/lần
                            </span>
                            <span className="inline-flex items-center gap-1 px-2.5 py-1 rounded-lg bg-white/15 text-indigo-100 font-medium">
                              Mã kích hoạt: <span className="font-mono text-white font-bold">{customerScanned.identifier}</span>
                            </span>
                          </div>
                        </div>
                      </div>

                      <button
                        type="button"
                        onClick={() => setCustomerScanned(null)}
                        className="p-1.5 text-white/70 hover:text-white rounded-lg hover:bg-white/10 transition cursor-pointer shrink-0"
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
