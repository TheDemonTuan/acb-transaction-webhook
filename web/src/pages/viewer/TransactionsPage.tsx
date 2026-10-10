import React, { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { fetchTransactions } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { isPublicViewerHost } from '../../app/runtime-mode';
import { formatVndCurrency } from '../../shared/formatters/money';
import { CashierTerminal } from '../../features/payment-qr/CashierTerminal';
import { TransactionHistoryView } from './TransactionHistoryView';

export type ViewerPageTab = 'cashier' | 'history';



function getInitialTab(): ViewerPageTab {
  if (typeof window === 'undefined') return 'cashier';
  const hash = window.location.hash.toLowerCase();
  const params = new URLSearchParams(window.location.search);
  const tabParam = params.get('tab');

  if (hash === '#transactions-history' || tabParam === 'history') {
    return 'history';
  }
  return 'cashier';
}

export const TransactionsPage: React.FC = () => {
  const isPublic = isPublicViewerHost();
  const [activeTab, setActiveTab] = useState<ViewerPageTab>(getInitialTab);

  // Sync tab with URL hash / params
  useEffect(() => {
    const handleSync = () => {
      setActiveTab(getInitialTab());
    };
    window.addEventListener('hashchange', handleSync);
    window.addEventListener('popstate', handleSync);
    return () => {
      window.removeEventListener('hashchange', handleSync);
      window.removeEventListener('popstate', handleSync);
    };
  }, []);

  const handleTabSelect = (tab: ViewerPageTab) => {
    setActiveTab(tab);
    const targetHash = tab === 'history' ? '#transactions-history' : '#counter-checkout';
    if (window.location.hash !== targetHash) {
      window.history.replaceState(null, '', targetHash);
    }
  };

  // Quick summary for tab badge
  const todaySummary = useQuery({
    queryKey: queryKeys.transactions({ from: new Date().toISOString().slice(0, 10), to: new Date().toISOString().slice(0, 10) }),
    queryFn: () => fetchTransactions({ from: new Date().toISOString().slice(0, 10), to: new Date().toISOString().slice(0, 10) }),
    staleTime: 30_000,
    refetchInterval: 30_000,
  });

  const todayCount = todaySummary.data?.summary?.count ?? todaySummary.data?.items?.length ?? 0;
  const todayIncoming = todaySummary.data?.summary?.incoming ?? 0;

  return (
    <div className="space-y-4 min-w-0">
      {/* Top Level Segmented Tab Switcher */}
      <div className="flex flex-wrap items-center justify-between gap-3 bg-white p-2.5 rounded-2xl border border-stone-200/90 shadow-xs">
        <div
          role="tablist"
          aria-label="Điều hướng chính"
          className="flex items-center gap-1.5 bg-stone-100/90 p-1 rounded-xl"
        >
          <button
            type="button"
            role="tab"
            aria-selected={activeTab === 'cashier'}
            onClick={() => handleTabSelect('cashier')}
            className={`flex items-center gap-2 px-3.5 py-2 rounded-lg text-xs font-bold transition cursor-pointer ${
              activeTab === 'cashier'
                ? 'bg-emerald-700 text-white shadow-xs'
                : 'text-stone-600 hover:text-stone-900'
            }`}
          >
            <span>⚡ Quầy thu ngân</span>
          </button>

          <button
            type="button"
            role="tab"
            aria-selected={activeTab === 'history'}
            onClick={() => handleTabSelect('history')}
            className={`flex items-center gap-2 px-3.5 py-2 rounded-lg text-xs font-bold transition cursor-pointer ${
              activeTab === 'history'
                ? 'bg-stone-900 text-white shadow-xs'
                : 'text-stone-600 hover:text-stone-900'
            }`}
          >
            <span>📋 Lịch sử giao dịch</span>
            <span
              className={`px-1.5 py-0.2 rounded-full text-[10px] font-extrabold ${
                activeTab === 'history'
                  ? 'bg-white/20 text-white'
                  : 'bg-stone-200 text-stone-700'
              }`}
            >
              {todayCount}
            </span>
          </button>
        </div>

        {/* Right Info Snippet */}
        <div className="text-xs text-stone-600 font-medium px-2 hidden sm:flex items-center gap-2">
          <span>Tiền vào hôm nay:</span>
          <strong className="text-emerald-800 font-extrabold text-sm">
            {isPublic ? '****** ₫' : formatVndCurrency(todayIncoming)}
          </strong>
        </div>
      </div>

      {/* Completely separated tab views */}
      {/* Completely separated tab views with state preservation */}
      <div className={activeTab === 'cashier' ? 'block min-w-0' : 'hidden'} role="tabpanel" aria-label="Quầy thu ngân">
        <CashierTerminal />
      </div>
      <div className={activeTab === 'history' ? 'block min-w-0' : 'hidden'} role="tabpanel" aria-label="Lịch sử giao dịch">
        <TransactionHistoryView />
      </div>
    </div>
  );
};
