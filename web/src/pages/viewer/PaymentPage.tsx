import React, { useEffect, useRef, useState } from 'react';
import { useParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import {
  AlertCircle,
  Building2,
  Check,
  CheckCircle2,
  Copy,
  CreditCard,
  ExternalLink,
  Radio,
  RefreshCw,
  Sparkles,
  User,
} from 'lucide-react';
import { activatePaymentPolling, fetchPublicPaymentQR } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { useRealtimeContext } from '../../realtime/RealtimeProvider';
import type { BankTransactionCreditData } from '../../realtime/realtime.types';
import { formatVndCurrency } from '../../shared/formatters/money';
import {
  PAYMENT_PAGE_ALLOWED_SOURCES,
  parseCreditAmount,
  shouldAcceptLiveCredit,
} from '../../features/payment-qr/credit-filter';
import {
  isQRReadyToDisplay,
  selectQRImageURL,
} from '../../features/payment-qr/qr-payload';
import {
  isSafeActivationIdentifier,
  newActivationIdentifier,
} from '../../features/payment-qr/activation-qr';
import { acbDeeplink as buildAcbDeeplink } from '../../features/payment-qr/acb-deeplink';

export const PaymentPage: React.FC = () => {
  const { identifier: paramIdentifier } = useParams<{ identifier?: string }>();
  const effectiveIdentifierRef = useRef<string>(
    isSafeActivationIdentifier(paramIdentifier)
      ? (paramIdentifier as string)
      : newActivationIdentifier(),
  );
  const activatedRef = useRef(false);
  const sessionOpenedAtRef = useRef<number>(Date.now());
  const seenTxIdsRef = useRef<Set<string>>(new Set());
  const [copiedField, setCopiedField] = useState<string | null>(null);
  const [activationWarning, setActivationWarning] = useState<string | null>(null);

  const [receivedCredit, setReceivedCredit] = useState<{
    amount: number;
    description: string;
    transactionNumber: string;
    time: string;
  } | null>(null);

  // Activate tracking on mount (idempotent, StrictMode safe)
  useEffect(() => {
    if (activatedRef.current) return;
    activatedRef.current = true;
    const token = effectiveIdentifierRef.current;
    activatePaymentPolling(token).catch((err) => {
      // Degraded: client can still view QR and complete payment normally
      console.warn('payment tracking activation fallback:', err);
      setActivationWarning(
        'Không thể kích hoạt chế độ theo dõi tăng tốc. Giao dịch vẫn được ghi nhận bình thường nhưng có thể trễ hơn đôi chút.',
      );
    });
  }, []);

  // Load public payment QR configuration
  const {
    data: qrData,
    isLoading,
    isError,
    error,
    refetch: refetchQR,
  } = useQuery({
    queryKey: queryKeys.publicPaymentQR,
    queryFn: fetchPublicPaymentQR,
  });

  const [imageLoadFailed, setImageLoadFailed] = useState(false);

  // Reset image failure if imageURL changes
  const qrImageURL = selectQRImageURL(qrData);
  useEffect(() => {
    setImageLoadFailed(false);
  }, [qrImageURL]);

  // Telemetry log on metadata load failure
  useEffect(() => {
    if (isError) {
      console.error('[PaymentQR] QR_METADATA_LOAD_FAILED', {
        error,
        status: (error as any)?.status,
      });
    }
  }, [isError, error]);

  const handleImageError = () => {
    console.error('[PaymentQR] QR_IMAGE_LOAD_FAILED', {
      imageURL: qrImageURL,
    });
    setImageLoadFailed(true);
  };

  const { subscribe } = useRealtimeContext();

  // Listen for realtime payment confirmation
  useEffect(() => {
    const unsub = subscribe<BankTransactionCreditData>(
      'bank.transaction.credit',
      (envelope) => {
        const d = envelope.data;
        if (!d) return;

        const accepted = shouldAcceptLiveCredit(d, {
          sessionOpenedAt: sessionOpenedAtRef.current,
          seenIds: seenTxIdsRef.current,
          sources: PAYMENT_PAGE_ALLOWED_SOURCES,
          now: Date.now(),
        });
        if (!accepted) return;

        const parsedAmount = parseCreditAmount(d.credit);
        setReceivedCredit({
          amount: parsedAmount,
          description: d.description,
          transactionNumber: d.transactionNumber,
          time: new Intl.DateTimeFormat('vi-VN', {
            hour: '2-digit',
            minute: '2-digit',
            second: '2-digit',
          }).format(new Date()),
        });
      }
    );
    return () => {
      unsub();
    };
  }, [subscribe]);

  const qr = qrData?.qr;
  const isReady = isQRReadyToDisplay(qrData);

  const isConfigured = Boolean(qrData?.configured && qrData?.hasImage);
  const isMetadataFailed = isError;
  const isImageFailed = imageLoadFailed;
  const isNotConfigured = !isLoading && !isError && (!isConfigured || !qrImageURL);

  const copyToClipboard = (text: string, field: string) => {
    navigator.clipboard.writeText(text);
    setCopiedField(field);
    setTimeout(() => setCopiedField(null), 2000);
  };

  const directDeeplink = buildAcbDeeplink(qr?.accountNumber, qr?.accountName);

  return (
    <div className="min-h-screen bg-slate-950 text-slate-100 flex flex-col items-center justify-center p-4 selection:bg-indigo-500 selection:text-white">
      <div className="w-full max-w-md bg-slate-900 border border-slate-800 rounded-2xl shadow-2xl overflow-hidden">
        {/* Header */}
        <div className="bg-gradient-to-r from-blue-600 via-indigo-600 to-cyan-600 p-5 text-center relative overflow-hidden">
          <div className="relative z-10">
            <h1 className="text-xl font-bold tracking-tight text-white flex items-center justify-center gap-2">
              <CreditCard className="w-6 h-6" />
              Thanh toán VietQR
            </h1>
            <p className="text-blue-100 text-xs mt-1">
              Quét mã hoặc mở ứng dụng ngân hàng để chuyển khoản
            </p>
          </div>
          <div className="absolute -right-6 -top-6 w-24 h-24 bg-white/10 rounded-full blur-xl" />
        </div>

        {/* Content */}
        <div className="p-5 space-y-5">
          {activationWarning && (
            <div className="bg-amber-950/50 border border-amber-500/30 rounded-xl p-3 text-amber-200 text-xs leading-relaxed">
              {activationWarning}
            </div>
          )}

          {/* Payment Success View */}
          {receivedCredit ? (
            <div className="bg-emerald-950/60 border border-emerald-500/30 rounded-xl p-5 text-center space-y-3 animate-in fade-in zoom-in-95 duration-300">
              <div className="w-14 h-14 bg-emerald-500/20 text-emerald-400 rounded-full flex items-center justify-center mx-auto ring-4 ring-emerald-500/10">
                <CheckCircle2 className="w-8 h-8" />
              </div>
              <div>
                <span className="text-xs uppercase tracking-wider font-semibold text-emerald-400">
                  Phát hiện giao dịch nhận tiền mới
                </span>
                <div className="text-2xl font-bold text-emerald-300 mt-1">
                  +{formatVndCurrency(receivedCredit.amount)}
                </div>
                <p className="text-[11px] text-slate-400 mt-1.5 max-w-xs mx-auto">
                  Hệ thống vừa ghi nhận một khoản tiền vào tài khoản. Vui lòng chờ người bán xác nhận giao dịch của bạn.
                </p>
              </div>
              <div className="text-xs text-slate-400 space-y-1 pt-2 border-t border-emerald-900/50">
                {receivedCredit.description && (
                  <div>Nội dung: <span className="text-slate-200">{receivedCredit.description}</span></div>
                )}
                <div>Mã GD: <span className="font-mono text-slate-200">{receivedCredit.transactionNumber}</span></div>
                <div>Thời gian: <span className="text-slate-200">{receivedCredit.time}</span></div>
              </div>
            </div>
          ) : (
            <>
              {/* Deeplink Direct Bank App Button */}
              <div>
                <a
                  href={directDeeplink}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="w-full flex items-center justify-center gap-2 py-3 px-4 bg-gradient-to-r from-blue-600 to-indigo-600 hover:from-blue-500 hover:to-indigo-500 text-white font-medium text-sm rounded-xl shadow-lg shadow-blue-500/20 active:scale-[0.98] transition-all"
                >
                  <span>Mở ứng dụng ACB ONE</span>
                  <ExternalLink className="w-4 h-4 opacity-80" />
                </a>
              </div>

              {/* Divider */}
              <div className="relative flex items-center justify-center">
                <div className="border-t border-slate-800 w-full" />
                <span className="bg-slate-900 px-3 text-[11px] uppercase tracking-wider text-slate-500 font-medium">
                  hoặc quét mã VietQR
                </span>
              </div>

              {/* VietQR Image Area */}
              <div className="flex flex-col items-center">
                <div className="bg-white p-3 rounded-2xl shadow-md border border-slate-700/50 max-w-[260px] aspect-square flex items-center justify-center">
                  {isLoading ? (
                    <div className="w-56 h-56 flex flex-col items-center justify-center text-slate-400 text-xs space-y-2">
                      <RefreshCw className="w-6 h-6 animate-spin text-indigo-500" />
                      <span>Đang tải mã VietQR...</span>
                    </div>
                  ) : isMetadataFailed ? (
                    <div className="w-56 h-56 flex flex-col items-center justify-center p-4 text-center text-slate-600 text-xs space-y-2">
                      <AlertCircle className="w-8 h-8 text-amber-500 opacity-90" />
                      <p className="font-semibold text-slate-800">Không tải được mã QR</p>
                      <p className="text-[11px] text-slate-500">Vui lòng dùng thông tin tài khoản bên dưới</p>
                      <button
                        type="button"
                        onClick={() => refetchQR()}
                        className="mt-1 px-3 py-1 bg-slate-100 hover:bg-slate-200 text-slate-700 rounded-lg text-[11px] font-medium transition cursor-pointer"
                      >
                        Thử lại
                      </button>
                    </div>
                  ) : isNotConfigured ? (
                    <div className="w-56 h-56 flex flex-col items-center justify-center p-4 text-center text-slate-500 text-xs space-y-2">
                      <CreditCard className="w-8 h-8 opacity-40 text-slate-500" />
                      <p className="font-semibold text-slate-700">Mã QR chưa được cài đặt</p>
                      <p className="text-[11px] text-slate-500">Vui lòng dùng thông tin tài khoản bên dưới</p>
                    </div>
                  ) : isReady && qrImageURL && !isImageFailed ? (
                    <img
                      src={qrImageURL}
                      alt="VietQR Code"
                      className="w-full h-full object-contain rounded-lg"
                      onError={handleImageError}
                    />
                  ) : (
                    <div className="w-56 h-56 flex flex-col items-center justify-center p-4 text-center text-slate-600 text-xs space-y-2">
                      <AlertCircle className="w-8 h-8 text-amber-500 opacity-90" />
                      <p className="font-semibold text-slate-800">Không tải được hình ảnh mã QR</p>
                      <p className="text-[11px] text-slate-500">Vui lòng dùng thông tin tài khoản bên dưới</p>
                      <button
                        type="button"
                        onClick={() => {
                          setImageLoadFailed(false);
                          refetchQR();
                        }}
                        className="mt-1 px-3 py-1 bg-slate-100 hover:bg-slate-200 text-slate-700 rounded-lg text-[11px] font-medium transition cursor-pointer"
                      >
                        Thử lại
                      </button>
                    </div>
                  )}
                </div>
              </div>

              {/* Bank Account Info Card */}
              {qr && (
                <div className="bg-slate-950/70 border border-slate-800 rounded-xl p-3.5 space-y-2.5 text-xs">
                  <div className="flex items-center justify-between">
                    <div className="flex items-center gap-2 text-slate-400">
                      <Building2 className="w-4 h-4 text-blue-400" />
                      <span>Ngân hàng</span>
                    </div>
                    <span className="font-semibold text-slate-200">
                      {qr.bankName || 'ACB (Á Châu)'}
                    </span>
                  </div>

                  {qr.accountNumber && (
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-2 text-slate-400">
                        <CreditCard className="w-4 h-4 text-indigo-400" />
                        <span>Số tài khoản</span>
                      </div>
                      <div className="flex items-center gap-1.5">
                        <span className="font-mono font-bold text-slate-100 text-sm">
                          {qr.accountNumber}
                        </span>
                        <button
                          type="button"
                          onClick={() => copyToClipboard(qr.accountNumber, 'acc')}
                          className="p-1 hover:bg-slate-800 rounded text-slate-400 hover:text-slate-200 transition"
                          title="Sao chép số tài khoản"
                        >
                          {copiedField === 'acc' ? (
                            <Check className="w-3.5 h-3.5 text-emerald-400" />
                          ) : (
                            <Copy className="w-3.5 h-3.5" />
                          )}
                        </button>
                      </div>
                    </div>
                  )}

                  {qr.accountName && (
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-2 text-slate-400">
                        <User className="w-4 h-4 text-cyan-400" />
                        <span>Chủ tài khoản</span>
                      </div>
                      <div className="flex items-center gap-1.5">
                        <span className="font-semibold uppercase text-slate-200">
                          {qr.accountName}
                        </span>
                        <button
                          type="button"
                          onClick={() => copyToClipboard(qr.accountName, 'name')}
                          className="p-1 hover:bg-slate-800 rounded text-slate-400 hover:text-slate-200 transition"
                          title="Sao chép tên chủ tài khoản"
                        >
                          {copiedField === 'name' ? (
                            <Check className="w-3.5 h-3.5 text-emerald-400" />
                          ) : (
                            <Copy className="w-3.5 h-3.5" />
                          )}
                        </button>
                      </div>
                    </div>
                  )}
                </div>
              )}

              {/* Waiting Status Indicator */}
              <div className="flex items-center justify-center gap-2 py-2 text-xs text-amber-300/90 bg-amber-500/10 border border-amber-500/20 rounded-lg">
                <Radio className="w-3.5 h-3.5 animate-pulse text-amber-400" />
                <span className="font-medium">Đang chờ giao dịch...</span>
                <Sparkles className="w-3.5 h-3.5 text-amber-400" />
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
};
