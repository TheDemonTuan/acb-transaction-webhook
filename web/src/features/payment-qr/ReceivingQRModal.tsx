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
  ShieldCheck,
  RefreshCw,
  Radio,
  Receipt,
  AlertTriangle,
  AlertCircle,
  ChevronDown,
  ChevronUp,
  WifiOff,
  Zap,
  RotateCcw,
} from 'lucide-react';
import {
  fetchPaymentQR,
  fetchPaymentReadiness,
  fetchStatus,
  fetchTransactions,
  getDynamicPaymentQRURL,
  startPaymentActivity,
  stopPaymentActivity,
} from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { useRealtimeContext } from '../../realtime/RealtimeProvider';
import type {
  AuthChangedData,
  BankTransactionCreditData,
  ConnectionChangedData,
  PollCompletedData,
  RealtimeEnvelope,
} from '../../realtime/realtime.types';
import { formatVndCurrency } from '../../shared/formatters/money';
import { isPublicViewerHost } from '../../app/runtime-mode';

export interface LiveCreditAlert {
  id: string;
  transactionNumber: string;
  amount: number;
  description: string;
  timestamp: number;
  timeStr: string;
}

export interface QRHealthParams {
  isPublic: boolean;
  networkOnline: boolean;
  serverReachable: boolean;
  sseStatus: string;
  acbState: string;
  pollError: { type: 'warning' | 'critical'; message: string } | null;
  publicReadiness?: { ready: boolean; status: string } | null;
}

export interface QRHealthResult {
  isInternetDown: boolean;
  isGatewayDown: boolean;
  isSseBroken: boolean;
  isSseStale: boolean;
  isAcbCritical: boolean;
  isAcbWarning: boolean;
  hasCritical: boolean;
  hasWarning: boolean;
}

export function computeQRHealthState({
  isPublic,
  networkOnline,
  serverReachable,
  sseStatus,
  acbState,
  pollError,
  publicReadiness,
}: QRHealthParams): QRHealthResult {
  const isInternetDown = !networkOnline;
  const isGatewayDown = networkOnline && !serverReachable;
  const isSseBroken = sseStatus === 'DISCONNECTED';
  const isSseStale = sseStatus === 'STALE' || sseStatus === 'RECONNECTING';

  let isAcbCritical = false;
  let isAcbWarning = false;

  if (isPublic) {
    if (publicReadiness) {
      if (!publicReadiness.ready) {
        if (
          publicReadiness.status === 'AUTH_STARTING' ||
          publicReadiness.status === 'IN_PROGRESS' ||
          publicReadiness.status === 'PAUSED'
        ) {
          isAcbWarning = true;
        } else {
          isAcbCritical = true;
        }
      }
    }
  } else {
    isAcbCritical =
      acbState === 'AUTH_REQUIRED' ||
      acbState === 'DISCONNECTED' ||
      acbState === 'UNCONFIGURED' ||
      acbState === 'FAILED' ||
      acbState === 'EXPIRED' ||
      pollError?.type === 'critical';

    isAcbWarning =
      !isAcbCritical &&
      (acbState === 'AUTH_STARTING' ||
        acbState === 'PAUSED' ||
        acbState === 'IN_PROGRESS' ||
        pollError?.type === 'warning');
  }

  const hasCritical = isInternetDown || isGatewayDown || isAcbCritical || isSseBroken;
  const hasWarning = !hasCritical && (isSseStale || isAcbWarning);

  return {
    isInternetDown,
    isGatewayDown,
    isSseBroken,
    isSseStale,
    isAcbCritical,
    isAcbWarning,
    hasCritical,
    hasWarning,
  };
}

export function parseAmountThousandsToVnd(raw: string): number {
  const digits = raw.replace(/\D/g, '').replace(/^0+/, '');
  if (!digits) return 0;
  const thousands = parseInt(digits, 10);
  return isNaN(thousands) ? 0 : thousands * 1000;
}

export function computeBoostPhase(remainingSeconds: number): {
  phase: number;
  minSec: number;
  maxSec: number;
  label: string;
} {
  if (remainingSeconds > 120) {
    return { phase: 1, minSec: 1, maxSec: 3, label: '⚡ Kiểm tra rất nhanh · 1–3 giây' };
  }
  if (remainingSeconds > 60) {
    return { phase: 2, minSec: 3, maxSec: 6, label: '⚡ Kiểm tra nhanh · 3–6 giây' };
  }
  if (remainingSeconds > 0) {
    return { phase: 3, minSec: 6, maxSec: 10, label: '⚡ Kiểm tra nhanh · 6–10 giây' };
  }
  return { phase: 0, minSec: 20, maxSec: 30, label: 'Kiểm tra tiêu chuẩn · 20–30 giây' };
}

export function isCreditMatch(expectedAmountVnd: number, incomingAmount: number): boolean {
  if (expectedAmountVnd === 0) {
    return incomingAmount > 0;
  }
  return incomingAmount === expectedAmountVnd;
}

export type ModalWorkflowState = 'idle' | 'active';

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

  // Workflow FSM: idle -> active
  const [modalState, setModalState] = useState<ModalWorkflowState>('idle');
  const [amountInput, setAmountInput] = useState<string>('');
  const [targetAmountVnd, setTargetAmountVnd] = useState<number>(0);
  const [boostRemainingSec, setBoostRemainingSec] = useState<number>(0);
  const [boostDegraded, setBoostDegraded] = useState<boolean>(false);
  const [boostSessionId, setBoostSessionId] = useState<string | null>(null);
  const boostSessionIdRef = useRef<string | null>(null);
  const [isStartingBoost, setIsStartingBoost] = useState<boolean>(false);
  const isStartingBoostRef = useRef<boolean>(false);
  isStartingBoostRef.current = isStartingBoost;

  const amountInputRef = useRef<HTMLInputElement>(null);
  const modalStateRef = useRef<ModalWorkflowState>(modalState);
  modalStateRef.current = modalState;
  const targetAmountRef = useRef<number>(targetAmountVnd);
  targetAmountRef.current = targetAmountVnd;

  const isPublic = isPublicViewerHost();

  // Realtime health & status states
  const {
    status: sseStatus,
    lastHeartbeatAt,
    networkOnline,
    serverReachable,
    subscribe,
    forceReconnect,
  } = useRealtimeContext();

  const [acbState, setAcbState] = useState<string>('UNKNOWN');
  const [pollError, setPollError] = useState<{ type: 'warning' | 'critical'; message: string } | null>(null);
  const [lastPollSuccessAt, setLastPollSuccessAt] = useState<number | null>(null);
  const [showHealthDetails, setShowHealthDetails] = useState(false);

  const todayStr = useMemo(() => {
    return new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Ho_Chi_Minh' }).format(new Date());
  }, [isOpen]);

  // Load Payment QR settings
  const { data: qrData } = useQuery({
    queryKey: queryKeys.paymentQR,
    queryFn: fetchPaymentQR,
    enabled: isOpen,
  });

  // Authoritative system status (admin only)
  const { data: systemStatus } = useQuery({
    queryKey: queryKeys.status,
    queryFn: fetchStatus,
    enabled: isOpen && !isPublic,
  });

  // Public sanitized payment readiness query
  const { data: paymentReadiness } = useQuery({
    queryKey: queryKeys.paymentReadiness,
    queryFn: fetchPaymentReadiness,
    enabled: isOpen && isPublic,
    refetchInterval: 3000,
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

  // Hydrate from authoritative snapshot
  useEffect(() => {
    if (!isOpen || !systemStatus?.acb) return;
    if (systemStatus.acb.state) {
      setAcbState(systemStatus.acb.state);
    }
    if (systemStatus.acb.lastSuccessfulPollAt) {
      const parsed = new Date(systemStatus.acb.lastSuccessfulPollAt).getTime();
      if (!isNaN(parsed)) {
        setLastPollSuccessAt(parsed);
      }
    }
  }, [isOpen, systemStatus]);

  // Hydrate ACB state from public payment readiness when in public mode
  useEffect(() => {
    if (!isOpen || !isPublic || !paymentReadiness) return;
    if (paymentReadiness.status) {
      setAcbState(paymentReadiness.status);
    }
  }, [isOpen, isPublic, paymentReadiness]);

  // Reset state when modal opens
  useEffect(() => {
    if (!isOpen) return;
    setSessionOpenedAt(Date.now());
    setSessionCredits([]);
    setActiveAlert(null);
    setActiveTab('qr');
    seenTxIdsRef.current.clear();
    setPollError(null);
    setShowHealthDetails(false);
    setModalState('idle');
    setAmountInput('');
    setTargetAmountVnd(0);
    setBoostRemainingSec(0);
    setBoostDegraded(false);
    setBoostSessionId(null);
    boostSessionIdRef.current = null;
    setIsStartingBoost(false);
    void refetchTx();

    // Auto-focus amount input on modal open
    setTimeout(() => {
      amountInputRef.current?.focus();
    }, 100);
  }, [isOpen, refetchTx]);

  // Second-by-second ticker for relative time and boost countdown
  useEffect(() => {
    if (!isOpen) return;
    const interval = setInterval(() => {
      setNowTick(Date.now());
      setBoostRemainingSec((prev) => (prev > 0 ? prev - 1 : 0));
    }, 1000);
    return () => clearInterval(interval);
  }, [isOpen]);

  // Compute layered health state
  const {
    isInternetDown,
    isGatewayDown,
    isSseBroken,
    isSseStale,
    isAcbCritical,
    isAcbWarning,
    hasCritical,
    hasWarning,
  } = computeQRHealthState({
    isPublic,
    networkOnline,
    serverReachable,
    sseStatus,
    acbState,
    pollError,
    publicReadiness: paymentReadiness,
  });

  // Trigger boost activation
  const handleStartBoost = async (amountVnd: number) => {
    if (isStartingBoostRef.current || isAcbCritical) return;
    isStartingBoostRef.current = true;
    setIsStartingBoost(true);
    setTargetAmountVnd(amountVnd);
    setBoostDegraded(false);

    try {
      const status = await startPaymentActivity({ amountVnd });
      if (status && status.active) {
        setBoostSessionId(status.sessionId || null);
        boostSessionIdRef.current = status.sessionId || null;
        setBoostRemainingSec(status.expiresIn || 180);
        setBoostDegraded(false);
        setModalState('active');
      } else {
        setBoostSessionId(null);
        boostSessionIdRef.current = null;
        setBoostRemainingSec(0);
        setBoostDegraded(true);
        setModalState('active');
      }
    } catch (err: unknown) {
      const errorObj = err as { code?: string; status?: number; message?: string } | undefined;
      const isPaymentNotReady =
        errorObj?.code === 'PAYMENT_NOT_READY' ||
        errorObj?.status === 409 ||
        (typeof errorObj?.message === 'string' &&
          (errorObj.message.includes('MONITORING') || errorObj.message.includes('PAYMENT_NOT_READY')));

      if (isPaymentNotReady) {
        queryClient.invalidateQueries({ queryKey: queryKeys.paymentReadiness });
        setBoostSessionId(null);
        boostSessionIdRef.current = null;
        setBoostRemainingSec(0);
        setBoostDegraded(false);
        setModalState('idle');
      } else {
        // Degraded: keep UI moving to QR view, but indicate standard polling
        setBoostSessionId(null);
        boostSessionIdRef.current = null;
        setBoostRemainingSec(0);
        setBoostDegraded(true);
        setModalState('active');
      }
    } finally {
      isStartingBoostRef.current = false;
      setIsStartingBoost(false);
    }
  };

  const handleResetToIdle = () => {
    void stopPaymentActivity(boostSessionIdRef.current || undefined);
    setBoostSessionId(null);
    boostSessionIdRef.current = null;
    setModalState('idle');
    setAmountInput('');
    setTargetAmountVnd(0);
    setBoostRemainingSec(0);
    setBoostDegraded(false);
    isStartingBoostRef.current = false;
    setIsStartingBoost(false);
    setTimeout(() => {
      amountInputRef.current?.focus();
    }, 50);
  };

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

          // FSM: Match credit in ACTIVE state -> backend auto-stops boost, frontend immediately resets to IDLE
          if (modalStateRef.current === 'active') {
            if (isCreditMatch(targetAmountRef.current, creditVal)) {
              setBoostSessionId(null);
              boostSessionIdRef.current = null;
              setModalState('idle');
              setAmountInput('');
              setTargetAmountVnd(0);
              setBoostRemainingSec(0);
              setBoostDegraded(false);
              isStartingBoostRef.current = false;
              setIsStartingBoost(false);
              setTimeout(() => {
                amountInputRef.current?.focus();
              }, 50);
            }
          }
        }
      }
    );

    return () => unsub();
  }, [isOpen, subscribe, queryClient, sessionOpenedAt]);

  // Subscribe to poll.completed events for health diagnostics (admin only)
  useEffect(() => {
    if (!isOpen || isPublic) return;
    const unsub = subscribe<PollCompletedData>(
      'poll.completed',
      (envelope: RealtimeEnvelope<PollCompletedData>) => {
        const d = envelope.data;
        if (!d) return;

        if (d.status === 'AUTH_REQUIRED' || d.error === 'SESSION_EXPIRED') {
          setAcbState('AUTH_REQUIRED');
          setPollError({ type: 'critical', message: 'Phiên đăng nhập ACB đã hết hạn' });
        } else if (d.status === 'PARTIAL') {
          setPollError({ type: 'warning', message: 'ACB phản hồi một phần dữ liệu' });
        } else if (d.status === 'FAILED') {
          const errCode = d.classifier || d.error || 'ACB_FAILED';
          let message = 'Lỗi kết nối kiểm tra ACB';
          if (errCode.includes('MAINTENANCE')) message = 'ACB đang bảo trì hệ thống';
          else if (errCode.includes('RATE_LIMITED')) message = 'ACB giới hạn tần suất yêu cầu';
          else if (errCode.includes('TIMEOUT')) message = 'Quá thời gian chờ phản hồi từ ACB';
          else if (errCode.includes('NETWORK')) message = 'Lỗi mạng kết nối máy chủ ACB';

          setPollError({ type: 'warning', message });
        } else if (d.status === 'SUCCEEDED') {
          setPollError(null);
          setLastPollSuccessAt(Date.now());
          setAcbState('MONITORING');
        }
      }
    );
    return () => unsub();
  }, [isOpen, isPublic, subscribe]);

  // Subscribe to connection and auth changes (admin only)
  useEffect(() => {
    if (!isOpen || isPublic) return;
    const unsubConn = subscribe<ConnectionChangedData>(
      'connection.changed',
      (envelope: RealtimeEnvelope<ConnectionChangedData>) => {
        const d = envelope.data;
        if (!d?.state) return;
        setAcbState(d.state);
      }
    );

    const unsubAuth = subscribe<AuthChangedData>(
      'auth.changed',
      (envelope: RealtimeEnvelope<AuthChangedData>) => {
        const d = envelope.data;
        if (!d?.status) return;
        if (d.status === 'MONITORING' || d.status === 'VERIFIED' || d.status === 'SUCCESS') {
          setAcbState('MONITORING');
        } else if (d.status === 'IN_PROGRESS' || d.status === 'AUTH_STARTING') {
          setAcbState('AUTH_STARTING');
        } else if (d.status === 'FAILED' || d.status === 'EXPIRED' || d.status === 'AUTH_REQUIRED') {
          setAcbState('AUTH_REQUIRED');
        }
      }
    );

    return () => {
      unsubConn();
      unsubAuth();
    };
  }, [isOpen, isPublic, subscribe]);

  // Close wrapper: ensure active boost is stopped
  const handleClose = () => {
    void stopPaymentActivity(boostSessionIdRef.current || undefined);
    setBoostSessionId(null);
    boostSessionIdRef.current = null;
    onClose();
  };

  // Keyboard navigation: Escape to close, Enter to advance
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        handleClose();
      } else if (e.key === 'Enter') {
        if (modalState === 'idle') {
          e.preventDefault();
          if (isStartingBoostRef.current || isAcbCritical) return;
          const amountVnd = parseAmountThousandsToVnd(amountInput);
          void handleStartBoost(amountVnd);
        }
      }
    };
    if (isOpen) {
      window.addEventListener('keydown', handleKeyDown);
    }
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, [isOpen, handleClose, modalState, amountInput, isAcbCritical]);

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

  const heartbeatSec = lastHeartbeatAt
    ? Math.max(0, Math.floor((nowTick - lastHeartbeatAt.getTime()) / 1000))
    : null;
  const pollSec = lastPollSuccessAt
    ? Math.max(0, Math.floor((nowTick - lastPollSuccessAt) / 1000))
    : null;

  const rawHistoryItems = txData?.items || [];
  const previewAmountVnd = parseAmountThousandsToVnd(amountInput);
  const boostPhase = computeBoostPhase(boostRemainingSec);

  // Dynamic image URL: target amount or static
  const qrImageSrc = targetAmountVnd > 0
    ? getDynamicPaymentQRURL(targetAmountVnd)
    : qrData?.imageURL || '/api/public/v1/payment-qr/image';

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
                <span
                  className={`w-2 h-2 rounded-full ${
                    hasCritical
                      ? 'bg-rose-500 animate-ping'
                      : hasWarning
                        ? 'bg-amber-500 animate-pulse'
                        : 'bg-emerald-500 animate-pulse'
                  }`}
                />
                {hasCritical
                  ? (isAcbCritical ? 'Phiên ACB cần đăng nhập lại' : 'Kênh kiểm tra giao dịch đang có sự cố')
                  : hasWarning
                    ? 'Đang đồng bộ lại kết nối trực tiếp...'
                    : modalState === 'active'
                      ? `⚡ Đang tăng tốc kiểm tra (${boostRemainingSec}s)`
                      : 'Đang trực tiếp theo dõi biến động số dư tài khoản'}
              </p>
            </div>
          </div>
          <button
            type="button"
            onClick={handleClose}
            className="p-2 text-stone-400 hover:text-stone-700 rounded-xl hover:bg-stone-200/60 transition cursor-pointer"
            title="Đóng cửa sổ (ESC)"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        {/* Critical Alerts */}
        {isInternetDown && (
          <div className="px-5 py-3 bg-rose-600 text-white text-xs flex items-center justify-between gap-3 shrink-0">
            <div className="flex items-center gap-2">
              <WifiOff className="w-4 h-4 shrink-0" />
              <div>
                <strong className="font-bold uppercase tracking-wider">Thiết bị đã mất kết nối Internet</strong>
                <p className="text-rose-100 text-[11px] mt-0.5">
                  Không thể nhận cập nhật giao dịch realtime. Hệ thống sẽ tự kết nối lại khi có mạng.
                </p>
              </div>
            </div>
          </div>
        )}

        {!isInternetDown && isGatewayDown && (
          <div className="px-5 py-3 bg-rose-600 text-white text-xs flex items-center justify-between gap-3 shrink-0">
            <div className="flex items-center gap-2">
              <AlertCircle className="w-4 h-4 shrink-0" />
              <div>
                <strong className="font-bold uppercase tracking-wider">Không kết nối được máy chủ Gateway</strong>
                <p className="text-rose-100 text-[11px] mt-0.5">
                  Internet vẫn hoạt động nhưng máy chủ không phản hồi. Đang tự động thử lại...
                </p>
              </div>
            </div>
            <button
              type="button"
              onClick={forceReconnect}
              className="px-2.5 py-1 rounded-lg bg-white/20 hover:bg-white/30 text-white font-medium text-[11px] shrink-0 cursor-pointer"
            >
              Thử lại ngay
            </button>
          </div>
        )}

        {!isInternetDown && !isGatewayDown && isAcbCritical && (
          <div className="px-5 py-3 bg-rose-600 text-white text-xs flex items-center justify-between gap-3 shrink-0">
            <div className="flex items-center gap-2">
              <AlertTriangle className="w-4 h-4 shrink-0" />
              <div>
                <strong className="font-bold uppercase tracking-wider">Phiên đăng nhập ACB đã hết hạn / gián đoạn</strong>
                <p className="text-rose-100 text-[11px] mt-0.5">
                  Hệ thống không thể tự động kiểm tra giao dịch mới từ ACB. Cần đăng nhập lại ACB để tiếp tục.
                </p>
              </div>
            </div>
          </div>
        )}

        {!hasCritical && isSseStale && (
          <div className="px-5 py-2.5 bg-amber-500 text-white text-xs flex items-center justify-between gap-3 shrink-0">
            <div className="flex items-center gap-2">
              <RefreshCw className="w-4 h-4 shrink-0 animate-spin" />
              <div>
                <strong className="font-bold">Kênh cập nhật realtime bị gián đoạn</strong>
              </div>
            </div>
            <button
              type="button"
              onClick={forceReconnect}
              className="px-2 py-0.5 rounded bg-white/20 hover:bg-white/30 text-white font-medium text-[11px] shrink-0 cursor-pointer"
            >
              Kết nối lại
            </button>
          </div>
        )}

        {/* Mobile Navigation Tabs */}
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
        <div className="flex-1 min-h-0 overflow-hidden flex flex-col">
          <div className="grid grid-cols-1 md:grid-cols-12 flex-1 min-h-0 h-full">
            {/* LEFT COLUMN: Interactive Workflow (IDLE / ACTIVE) */}
            <div
              className={`md:col-span-5 p-5 sm:p-6 bg-stone-50/50 md:border-r border-stone-200/80 flex flex-col items-center justify-start text-center gap-4 min-h-0 h-full overflow-y-auto ${
                activeTab === 'qr' ? 'flex' : 'hidden md:flex'
              }`}
            >
              {/* Realtime Health Diagnostics Panel */}
              <div className="w-full max-w-[320px] rounded-2xl border border-stone-200 bg-white shadow-2xs overflow-hidden text-xs text-left transition">
                <button
                  type="button"
                  onClick={() => setShowHealthDetails((prev) => !prev)}
                  className="w-full p-2.5 flex items-center justify-between gap-2 hover:bg-stone-50 cursor-pointer transition"
                >
                  <div className="flex items-center gap-2 min-w-0">
                    <span
                      className={`w-2.5 h-2.5 rounded-full shrink-0 ${
                        hasCritical
                          ? 'bg-rose-500'
                          : hasWarning
                            ? 'bg-amber-500 animate-pulse'
                            : 'bg-emerald-500'
                      }`}
                    />
                    <span className="font-bold text-stone-800 truncate">
                      {hasCritical
                        ? (isAcbCritical ? 'Phiên ACB cần đăng nhập lại' : 'Không thể xác nhận tự động')
                        : hasWarning
                          ? 'Đang phục hồi kênh trực tiếp'
                          : 'Hệ thống sẵn sàng nhận tiền'}
                    </span>
                  </div>
                  <div className="flex items-center gap-1 text-[11px] text-stone-400 shrink-0">
                    <span>{showHealthDetails ? 'Thu gọn' : 'Chi tiết'}</span>
                    {showHealthDetails ? (
                      <ChevronUp className="w-3.5 h-3.5" />
                    ) : (
                      <ChevronDown className="w-3.5 h-3.5" />
                    )}
                  </div>
                </button>

                {showHealthDetails && (
                  <div className="px-3 pb-3 pt-1 border-t border-stone-100 space-y-1.5 text-[11px]">
                    <div className="flex items-center justify-between">
                      <span className="text-stone-500">Gateway</span>
                      <span className="font-medium flex items-center gap-1">
                        <span
                          className={`w-1.5 h-1.5 rounded-full ${serverReachable ? 'bg-emerald-500' : 'bg-rose-500'}`}
                        />
                        <span className={serverReachable ? 'text-stone-800' : 'text-rose-600 font-bold'}>
                          {serverReachable ? 'Đã kết nối' : 'Mất phản hồi'}
                        </span>
                      </span>
                    </div>

                    <div className="flex items-center justify-between">
                      <span className="text-stone-500">Realtime SSE</span>
                      <span className="font-medium flex items-center gap-1">
                        <span
                          className={`w-1.5 h-1.5 rounded-full ${
                            sseStatus === 'CONNECTED'
                              ? 'bg-emerald-500'
                              : sseStatus === 'STALE' || sseStatus === 'RECONNECTING'
                                ? 'bg-amber-500'
                                : 'bg-rose-500'
                          }`}
                        />
                        <span className="text-stone-800">
                          {sseStatus === 'CONNECTED'
                            ? `Trực tuyến${heartbeatSec !== null ? ` · ${heartbeatSec}s trước` : ''}`
                            : sseStatus === 'STALE'
                              ? 'Tín hiệu gián đoạn'
                              : sseStatus === 'RECONNECTING'
                                ? 'Đang kết nối lại'
                                : 'Mất kết nối'}
                        </span>
                      </span>
                    </div>

                    {isPublic ? (
                      <div className="flex items-center justify-between">
                        <span className="text-stone-500">Trạng thái nhận tiền</span>
                        <span className="font-medium flex items-center gap-1">
                          <span
                            className={`w-1.5 h-1.5 rounded-full ${!isAcbCritical ? 'bg-emerald-500' : 'bg-rose-500'}`}
                          />
                          <span className={!isAcbCritical ? 'text-stone-800' : 'text-rose-600 font-bold'}>
                            {isAcbCritical ? 'Cần đăng nhập lại' : isAcbWarning ? 'Đang kết nối...' : 'Sẵn sàng'}
                          </span>
                        </span>
                      </div>
                    ) : (
                      <>
                        <div className="flex items-center justify-between">
                          <span className="text-stone-500">Phiên ACB</span>
                          <span className="font-medium flex items-center gap-1">
                            <span
                              className={`w-1.5 h-1.5 rounded-full ${!isAcbCritical ? 'bg-emerald-500' : 'bg-rose-500'}`}
                            />
                            <span className={!isAcbCritical ? 'text-stone-800' : 'text-rose-600 font-bold'}>
                              {acbState === 'MONITORING'
                                ? 'Đang hoạt động'
                                : isAcbWarning
                                  ? 'Đang kết nối...'
                                  : isAcbCritical
                                    ? 'Cần đăng nhập lại'
                                    : 'Sẵn sàng'}
                            </span>
                          </span>
                        </div>

                        <div className="flex items-center justify-between">
                          <span className="text-stone-500">Kiểm tra giao dịch</span>
                          <span className="font-medium flex items-center gap-1">
                            <span
                              className={`w-1.5 h-1.5 rounded-full ${
                                pollError
                                  ? pollError.type === 'critical'
                                    ? 'bg-rose-500'
                                    : 'bg-amber-500'
                                  : 'bg-emerald-500'
                              }`}
                            />
                            <span className="text-stone-800 truncate max-w-[160px]">
                              {pollError
                                ? pollError.message
                                : pollSec !== null
                                  ? `OK · ${pollSec}s trước`
                                  : 'Sẵn sàng'}
                            </span>
                          </span>
                        </div>
                      </>
                    )}
                  </div>
                )}
              </div>

              {/* FSM STATE 1: IDLE - Amount Input Form */}
              {modalState === 'idle' && (
                <div className="w-full max-w-[320px] bg-white rounded-3xl p-5 sm:p-6 border border-stone-200 shadow-xs flex flex-col items-center text-center space-y-4 animate-in fade-in zoom-in-95 duration-150">
                  <div className="w-12 h-12 rounded-2xl bg-emerald-50 text-emerald-600 flex items-center justify-center border border-emerald-100">
                    <Zap className="w-6 h-6" />
                  </div>
                  <div>
                    <h4 className="font-bold text-stone-900 text-sm">Nhập số tiền nhận</h4>
                    <p className="text-[11px] text-stone-500 mt-0.5">
                      Đơn vị nghìn đồng (nhập 100 = 100.000 ₫)
                    </p>
                  </div>

                  <div className="w-full space-y-2">
                    <div className="relative">
                      <input
                        ref={amountInputRef}
                        type="text"
                        inputMode="numeric"
                        autoFocus
                        value={amountInput}
                        onChange={(e) => {
                          const val = e.target.value.replace(/\D/g, '').replace(/^0+/, '');
                          setAmountInput(val);
                        }}
                        placeholder="Ví dụ: 100"
                        className="w-full px-4 py-3 rounded-xl border border-stone-300 focus:border-emerald-600 focus:ring-2 focus:ring-emerald-500/20 text-center font-mono font-bold text-xl text-stone-900 placeholder:text-stone-300 outline-none transition"
                      />
                      <span className="absolute right-3.5 top-1/2 -translate-y-1/2 text-xs font-bold text-stone-400">
                        .000 ₫
                      </span>
                    </div>

                    <div className="min-h-[20px] text-xs">
                      {previewAmountVnd > 0 ? (
                        <p className="font-black text-emerald-600 text-sm animate-in fade-in duration-100">
                          = {formatVndCurrency(previewAmountVnd)}
                        </p>
                      ) : (
                        <p className="text-[11px] text-stone-400">Chưa cố định số tiền</p>
                      )}
                    </div>
                  </div>

                  <div className="w-full space-y-2 pt-1">
                    <button
                      type="button"
                      disabled={isStartingBoost || isAcbCritical}
                      onClick={() => handleStartBoost(previewAmountVnd)}
                      className={`w-full py-3 px-4 rounded-xl font-bold text-xs shadow-sm transition flex items-center justify-center gap-2 ${
                        isAcbCritical
                          ? 'bg-stone-200 text-stone-400 cursor-not-allowed border border-stone-300'
                          : 'bg-emerald-600 hover:bg-emerald-700 disabled:opacity-60 text-white hover:shadow cursor-pointer'
                      }`}
                    >
                      <QrCode className="w-4 h-4" />
                      {isAcbCritical
                        ? 'Không thể nhận tiền lúc này'
                        : isStartingBoost
                          ? 'Đang kích hoạt...'
                          : 'Tạo QR & bắt đầu nhận tiền'}
                    </button>

                    {!isAcbCritical && (
                      <button
                        type="button"
                        disabled={isStartingBoost}
                        onClick={() => handleStartBoost(0)}
                        className="w-full py-2 px-3 text-[11px] text-stone-500 hover:text-stone-800 disabled:opacity-60 font-medium transition cursor-pointer"
                      >
                        Hoặc: Nhận tiền không cố định số tiền (Enter)
                      </button>
                    )}
                  </div>
                </div>
              )}

              {/* FSM STATE 2: ACTIVE - Dynamic QR & Live Boost Countdown */}
              {modalState === 'active' && isConfigured && (
                <div className="w-full max-w-[320px] flex flex-col items-center space-y-3 animate-in fade-in zoom-in-95 duration-150">
                  {/* Boost Phase Indicator Header */}
                  {boostRemainingSec > 0 ? (
                    <div className="w-full bg-amber-500 text-white rounded-2xl p-2.5 shadow-xs flex items-center justify-between text-xs px-3.5">
                      <div className="flex items-center gap-1.5 font-bold">
                        <Zap className="w-3.5 h-3.5 fill-white animate-pulse" />
                        <span>{boostPhase.label}</span>
                      </div>
                      <span className="text-[11px] font-mono bg-amber-600/60 px-2 py-0.5 rounded-full font-bold">
                        {boostRemainingSec}s
                      </span>
                    </div>
                  ) : (
                    <div className="w-full bg-stone-100 border border-stone-200 text-stone-700 rounded-2xl p-2.5 shadow-2xs flex items-center gap-2 text-xs px-3.5">
                      <span className="font-medium">{boostPhase.label}</span>
                    </div>
                  )}

                  {/* High-Contrast QR Code Card */}
                  <div className="w-full bg-white rounded-3xl p-4 sm:p-5 border border-stone-200 shadow-sm relative group">
                    <div className="aspect-square bg-white flex items-center justify-center overflow-hidden rounded-2xl">
                      <img
                        src={qrImageSrc}
                        alt="Mã QR nhận tiền ACB"
                        className="w-full h-full object-contain"
                      />
                    </div>

                    {targetAmountVnd > 0 && (
                      <div className="mt-3 pt-2.5 border-t border-stone-100">
                        <span className="text-[10px] text-stone-400 uppercase font-semibold">Số tiền cần nhận</span>
                        <p className="text-xl font-black text-emerald-700 font-mono tracking-tight">
                          {formatVndCurrency(targetAmountVnd)}
                        </p>
                      </div>
                    )}
                  </div>

                  {/* Account Information with Copy Button */}
                  <div className="w-full bg-white rounded-2xl p-3.5 border border-stone-200 shadow-2xs text-xs text-left space-y-2">
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

                  {/* Action / Reset Button */}
                  <button
                    type="button"
                    onClick={handleResetToIdle}
                    className="text-xs text-stone-500 hover:text-stone-800 flex items-center gap-1.5 py-1 px-3 rounded-lg hover:bg-stone-200/50 transition cursor-pointer"
                  >
                    <RotateCcw className="w-3.5 h-3.5" />
                    Đổi số tiền / Hủy phiên này
                  </button>
                </div>
              )}

              {modalState === 'active' && !isConfigured && (
                <div className="py-16 space-y-3 text-stone-400">
                  <QrCode className="w-16 h-16 mx-auto stroke-1" />
                  <p className="text-sm font-bold text-stone-700">Chưa thiết lập mã QR nhận tiền</p>
                  <p className="text-xs text-stone-500 max-w-xs mx-auto">
                    Vào phần Quản trị &rarr; Kết nối ACB để tải ảnh QR hoặc tạo VietQR tự động.
                  </p>
                </div>
              )}
            </div>

            {/* RIGHT COLUMN: Live Monitor Status & Recent Transactions History */}
            <div
              className={`md:col-span-7 p-5 sm:p-6 flex flex-col space-y-4 min-h-0 h-full overflow-hidden ${
                activeTab === 'history' ? 'flex' : 'hidden md:flex'
              }`}
            >
              {/* TOP: Live Status & Active Alert Banner */}
              <div className="space-y-3 shrink-0">
                {activeAlert && (
                  <div className="bg-emerald-500 text-white rounded-2xl p-4 sm:p-5 shadow-lg border-2 border-emerald-400 text-left animate-in slide-in-from-top-3 fade-in duration-200 relative overflow-hidden">
                    <div className="absolute -right-6 -bottom-6 w-36 h-36 bg-white/10 rounded-full blur-2xl pointer-events-none" />
                    <div className="flex items-start justify-between gap-3 relative z-10">
                      <div className="flex items-start gap-3.5">
                        <div className="w-12 h-12 rounded-2xl bg-white text-emerald-600 flex items-center justify-center shrink-0 shadow-md">
                          <CheckCircle2 className="w-7 h-7" />
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
                )}

                {/* Waiting card when no active alert */}
                {!activeAlert && (
                  <div className="p-4 rounded-2xl bg-stone-50 border border-stone-200/80 flex items-center justify-between gap-3 text-left">
                    <div className="flex items-center gap-3">
                      <div className="w-10 h-10 rounded-xl bg-emerald-50 text-emerald-600 border border-emerald-100 flex items-center justify-center shrink-0">
                        <Radio className="w-5 h-5 animate-pulse" />
                      </div>
                      <div>
                        <p className="text-xs font-bold text-stone-900 flex items-center gap-1.5">
                          {modalState === 'active'
                            ? `Đang tăng tốc kiểm tra: còn ${boostRemainingSec}s...`
                            : 'Đang chờ khách hàng chuyển khoản...'}
                        </p>
                        <p className="text-[11px] text-stone-500 mt-0.5">
                          Phiên mở lúc {new Date(sessionOpenedAt).toLocaleTimeString('vi-VN')} • Tự động báo ngay khi tài khoản có tiền
                        </p>
                      </div>
                    </div>
                  </div>
                )}
              </div>

              {/* Transactions History List */}
              <div className="flex-1 min-h-0 flex flex-col pt-2 overflow-hidden">
                <div className="flex items-center justify-between pb-2 border-b border-stone-100 shrink-0">
                  <h4 className="font-bold text-xs text-stone-800 uppercase tracking-wider flex items-center gap-1.5">
                    <Receipt className="w-3.5 h-3.5 text-stone-400" />
                    Lịch sử nhận tiền hôm nay
                  </h4>
                  <span className="text-[11px] text-stone-400 font-medium">
                    {rawHistoryItems.length} giao dịch
                  </span>
                </div>

                <div className="flex-1 min-h-0 overflow-y-auto divide-y divide-stone-100 mt-2 pr-1">
                  {rawHistoryItems.length === 0 ? (
                    <div className="py-12 text-center text-stone-400 text-xs">
                      Chưa có giao dịch nhận tiền nào hôm nay.
                    </div>
                  ) : (
                    rawHistoryItems.map((item) => (
                      <div
                        key={item.id}
                        className="py-2.5 flex items-center justify-between text-xs hover:bg-stone-50/60 px-2 rounded-xl transition"
                      >
                        <div className="text-left space-y-0.5 min-w-0 pr-3">
                          <p className="font-medium text-stone-800 truncate">
                            {item.description || 'Chuyển khoản'}
                          </p>
                          <p className="text-[11px] text-stone-400 font-mono">
                            {item.transactionDate || ''}
                          </p>
                        </div>
                        <div className="text-right shrink-0">
                          <span className="font-bold text-emerald-600 font-mono">
                            +{formatVndCurrency(item.credit)}
                          </span>
                        </div>
                      </div>
                    ))
                  )}
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
};
