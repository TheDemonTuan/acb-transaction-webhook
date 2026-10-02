import React, { createContext, useContext, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { sendConnectionAction } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';

export interface BankConnectionContextValue {
  globalNotice: { kind: 'ok' | 'error'; text: string } | null;
  setGlobalNotice: (notice: { kind: 'ok' | 'error'; text: string } | null) => void;
  sync: () => Promise<void>;
  isSyncing: boolean;
}

const BankConnectionContext = createContext<BankConnectionContextValue | null>(null);

export const BankConnectionProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const queryClient = useQueryClient();
  const [globalNotice, setGlobalNotice] = useState<{ kind: 'ok' | 'error'; text: string } | null>(null);
  const [isSyncing, setIsSyncing] = useState(false);

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

  const value: BankConnectionContextValue = {
    globalNotice,
    setGlobalNotice,
    sync,
    isSyncing,
  };

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
