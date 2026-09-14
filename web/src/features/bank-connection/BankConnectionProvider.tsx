import React, { createContext, useContext, useEffect, useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import {
  cancelAuthSession,
  checkAuthStatus,
  fetchCurrentAuthSession,
  sendConnectionAction,
  startAuthSession,
} from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { isTerminalAuthError } from '../../api';

export interface ActiveAttempt {
  id: string;
  screenUrl: string;
  browserUnavailable?: boolean;
}

export interface BankConnectionContextValue {
  activeAttempt: ActiveAttempt | null;
  hasActiveAuth: boolean;
  browserUnavailable: boolean;
  globalNotice: { kind: 'ok' | 'error'; text: string } | null;
  setGlobalNotice: (notice: { kind: 'ok' | 'error'; text: string } | null) => void;
  startAuth: () => Promise<void>;
  cancelAuth: () => Promise<void>;
  sync: () => Promise<void>;
  reloadScreen: () => void;
  screenKey: number;
  isStartingAuth: boolean;
  isCancelling: boolean;
  isSyncing: boolean;
}

const BankConnectionContext = createContext<BankConnectionContextValue | null>(null);

export const BankConnectionProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const queryClient = useQueryClient();
  const [activeAttempt, setActiveAttempt] = useState<ActiveAttempt | null>(null);
  const [browserUnavailable, setBrowserUnavailable] = useState(false);
  const [screenKey, setScreenKey] = useState(0);
  const [globalNotice, setGlobalNotice] = useState<{ kind: 'ok' | 'error'; text: string } | null>(null);
  const [isStartingAuth, setIsStartingAuth] = useState(false);
  const [isCancelling, setIsCancelling] = useState(false);
  const [isSyncing, setIsSyncing] = useState(false);

  const reloadScreen = () => {
    setScreenKey((prev) => prev + 1);
  };

  // Recover active auth session on mount across reloads
  useEffect(() => {
    let cancelled = false;
    fetchCurrentAuthSession()
      .then((res) => {
        if (!cancelled && res?.attempt) {
          const isUnavailable = Boolean(res.attempt.browserUnavailable);
          setActiveAttempt({
            id: res.attempt.attemptId,
            screenUrl: res.attempt.screenUrl,
            browserUnavailable: isUnavailable,
          });
          setBrowserUnavailable(isUnavailable);
        }
      })
      .catch(() => {
        // Silently ignore if not configured or unauthenticated
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const startAuth = async () => {
    setIsStartingAuth(true);
    setGlobalNotice(null);
    setBrowserUnavailable(false);
    try {
      const res = await startAuthSession();
      setActiveAttempt({ id: res.attemptId, screenUrl: res.screenUrl, browserUnavailable: false });
    } catch (err: any) {
      setGlobalNotice({
        kind: 'error',
        text: err instanceof Error ? err.message : 'Không thể bắt đầu phiên đăng nhập.',
      });
    } finally {
      setIsStartingAuth(false);
    }
  };

  const cancelAuth = async () => {
    if (!activeAttempt) return;
    setIsCancelling(true);
    try {
      await cancelAuthSession(activeAttempt.id);
      setActiveAttempt(null);
      setBrowserUnavailable(false);
      setGlobalNotice({ kind: 'ok', text: 'Đã hủy phiên đăng nhập ACB.' });
      queryClient.invalidateQueries({ queryKey: queryKeys.connection });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
    } catch (err: any) {
      setGlobalNotice({
        kind: 'error',
        text: err instanceof Error ? err.message : 'Không thể hủy phiên đăng nhập.',
      });
    } finally {
      setIsCancelling(false);
    }
  };

  const sync = async () => {
    setIsSyncing(true);
    try {
      await sendConnectionAction('sync');
      setGlobalNotice({ kind: 'ok', text: 'Đã tiếp nhận yêu cầu đồng bộ ACB.' });
      queryClient.invalidateQueries({ queryKey: queryKeys.connection });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
      queryClient.invalidateQueries({ queryKey: ['transactions'] });
    } catch (err: any) {
      const msg = err instanceof Error ? err.message : 'Không thể đồng bộ.';
      setGlobalNotice({
        kind: 'error',
        text: msg,
      });
      throw err;
    } finally {
      setIsSyncing(false);
    }
  };

  // Background polling loop for active auth session across all tabs
  useEffect(() => {
    if (!activeAttempt) return;
    let cancelled = false;
    let checking = false;
    let timer: number | undefined;

    const check = async () => {
      if (checking || cancelled) return;
      checking = true;
      try {
        const result = await checkAuthStatus(activeAttempt.id);
        if (cancelled) return;
        setBrowserUnavailable(false);
        setActiveAttempt((prev) => (prev ? { ...prev, browserUnavailable: false } : null));
        if (result.status === 'MONITORING' || result.status === 'VERIFIED') {
          cancelled = true;
          setActiveAttempt(null);
          setBrowserUnavailable(false);
          setGlobalNotice({
            kind: 'ok',
            text: 'ACB đã xác thực. Hệ thống đang bắt đầu theo dõi giao dịch.',
          });
          queryClient.invalidateQueries({ queryKey: queryKeys.connection });
          queryClient.invalidateQueries({ queryKey: queryKeys.status });
          return;
        }
        if (
          result.status === 'FAILED' ||
          result.status === 'EXPIRED' ||
          result.status === 'CANCELLED'
        ) {
          cancelled = true;
          setActiveAttempt(null);
          setBrowserUnavailable(false);
          if (result.status === 'CANCELLED') {
            setGlobalNotice({ kind: 'ok', text: 'Đã hủy phiên đăng nhập ACB.' });
          } else {
            setGlobalNotice({
              kind: 'error',
              text: result.error ?? 'Phiên đăng nhập ACB đã kết thúc. Vui lòng mở phiên mới.',
            });
          }
          queryClient.invalidateQueries({ queryKey: queryKeys.connection });
          queryClient.invalidateQueries({ queryKey: queryKeys.status });
          return;
        }
      } catch (error: any) {
        if (cancelled) return;
        if (isTerminalAuthError(error)) {
          cancelled = true;
          setActiveAttempt(null);
          setBrowserUnavailable(false);
          setGlobalNotice({
            kind: 'error',
            text:
              error.code === 'AUTH_SESSION_SUPERSEDED'
                ? 'Phiên đăng nhập ACB đã được thay thế. Vui lòng mở phiên mới.'
                : error.code === 'AUTH_SESSION_NOT_FOUND'
                ? 'Không tìm thấy phiên đăng nhập ACB. Vui lòng mở phiên mới.'
                : error.message,
          });
          queryClient.invalidateQueries({ queryKey: queryKeys.connection });
          queryClient.invalidateQueries({ queryKey: queryKeys.status });
          return;
        }
        setBrowserUnavailable(true);
        setActiveAttempt((prev) => (prev ? { ...prev, browserUnavailable: true } : null));
        setGlobalNotice({
          kind: 'error',
          text: error instanceof Error ? error.message : 'Không thể kiểm tra trạng thái đăng nhập ACB.',
        });
      } finally {
        checking = false;
        if (!cancelled) timer = window.setTimeout(() => void check(), 1000);
      }
    };

    void check();
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
    };
  }, [activeAttempt?.id, queryClient]);

  const value = useMemo(
    () => ({
      activeAttempt,
      hasActiveAuth: activeAttempt !== null || isStartingAuth,
      browserUnavailable,
      globalNotice,
      setGlobalNotice,
      startAuth,
      cancelAuth,
      sync,
      reloadScreen,
      screenKey,
      isStartingAuth,
      isCancelling,
      isSyncing,
    }),
    [activeAttempt, browserUnavailable, globalNotice, isStartingAuth, isCancelling, isSyncing, screenKey]
  );

  return (
    <BankConnectionContext.Provider value={value}>
      {children}
    </BankConnectionContext.Provider>
  );
};

export function useBankConnection(): BankConnectionContextValue {
  const ctx = useContext(BankConnectionContext);
  if (!ctx) {
    throw new Error('useBankConnection must be used within a BankConnectionProvider');
  }
  return ctx;
}
