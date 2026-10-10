import React, { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import {
  Search,
  ArrowDownLeft,
  ArrowUpRight,
  RefreshCw,
  Clock,
  ChevronRight,
  Receipt,
  Copy,
  Check,
  X,
  Calendar,
} from 'lucide-react';
import { fetchTransactions } from '../../shared/api/queries';
import { isPublicViewerHost } from '../../app/runtime-mode';
import { ROUTES } from '../../app/routes';
import { queryKeys } from '../../shared/api/query-keys';
import { formatVndCurrency } from '../../shared/formatters/money';
import { formatDateTimeVN } from '../../shared/formatters/datetime';
import type { Transaction } from '../../realtime-types';
import { useCursorPagination, PaginationControls } from '../../shared/ui/PaginationControls';
import { CounterCheckout } from '../../features/payment-qr/CounterCheckout';

const getTodayISO = () => {
  const d = new Date();
  const year = d.getFullYear();
  const month = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
};

const getDaysAgoISO = (days: number) => {
  const d = new Date();
  d.setDate(d.getDate() - days);
  const year = d.getFullYear();
  const month = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
};

export const TransactionsPage: React.FC = () => {
  const [search, setSearch] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const isPublic = isPublicViewerHost();
  const [filterType, setFilterType] = useState<'all' | 'credit' | 'debit'>(isPublic ? 'credit' : 'all');
  // Default filter to 'today' as requested by user
  const [dateRange, setDateRange] = useState<'today' | '7days' | 'all' | 'custom'>('today');
  const [customFrom, setCustomFrom] = useState('');
  const [customTo, setCustomTo] = useState('');
  const [selectedTx, setSelectedTx] = useState<Transaction | null>(null);
  const [copiedId, setCopiedId] = useState(false);
  const pagination = useCursorPagination(20);
  const resetPagination = pagination.reset;
  const appliedSearchRef = useRef('');

  // Debounce search input without resetting the initial cursor asynchronously.
  useEffect(() => {
    const timer = setTimeout(() => {
      const nextSearch = search.trim();
      setDebouncedSearch(nextSearch);
      if (nextSearch !== appliedSearchRef.current) {
        appliedSearchRef.current = nextSearch;
        resetPagination();
      }
    }, 300);
    return () => clearTimeout(timer);
  }, [search, resetPagination]);

  // Reset cursor when filters change
  const handleDateRangeChange = (range: 'today' | '7days' | 'all' | 'custom') => {
    setDateRange(range);
    pagination.reset();
  };

  const handleFilterTypeChange = (type: 'all' | 'credit' | 'debit') => {
    setFilterType(type);
    pagination.reset();
  };

  const queryParams = useMemo(() => {
    let from: string | undefined;
    let to: string | undefined;
    if (dateRange === 'today') {
      from = getTodayISO();
      to = getTodayISO();
    } else if (dateRange === '7days') {
      from = getDaysAgoISO(6);
      to = getTodayISO();
    } else if (dateRange === 'custom') {
      from = customFrom || undefined;
      to = customTo || undefined;
    }
    return {
      from,
      to,
      direction: isPublic ? 'credit' as const : filterType,
      query: debouncedSearch.trim() || undefined,
      limit: pagination.pageSize,
      cursor: pagination.cursor,
    };
  }, [isPublic, dateRange, customFrom, customTo, filterType, debouncedSearch, pagination.pageSize, pagination.cursor]);

  const { data, isLoading, isRefetching, isError, error, refetch } = useQuery({
    queryKey: queryKeys.transactions(queryParams as unknown as Record<string, unknown>),
    queryFn: () => fetchTransactions(queryParams),
  });

  const transactions = data?.items || [];
  const summary = data?.summary;

  const stats = useMemo(() => {
    return {
      totalCount: summary?.count ?? transactions.length,
      incoming: summary?.incoming ?? 0,
      outgoing: summary?.outgoing ?? 0,
    };
  }, [summary, transactions.length]);

  const handleCopy = (text: string) => {
    if (typeof navigator !== 'undefined') {
      navigator.clipboard.writeText(text);
      setCopiedId(true);
      setTimeout(() => setCopiedId(false), 2000);
    }
  };

  return (
    <div className="space-y-6 min-w-0">
      {/* Quick Navigation Bar */}
      <div className="flex flex-wrap items-center justify-between gap-3 bg-white px-3 py-2 rounded-2xl border border-stone-200 shadow-2xs">
        <div className="flex items-center gap-2">
          <a
            href="#counter-checkout"
            className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-bold bg-emerald-700 text-white shadow-xs hover:bg-emerald-800 transition"
          >
            <span>⚡ Quầy thu ngân</span>
          </a>
          <a
            href="#transactions-history"
            className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-semibold bg-stone-100 text-stone-700 hover:bg-stone-200/70 transition"
          >
            <span>📋 Lịch sử giao dịch ({stats.totalCount})</span>
          </a>
        </div>
        <div className="text-xs text-stone-600 font-medium px-2 hidden sm:flex items-center gap-2">
          <span>Tiền vào hôm nay:</span>
          <strong className="text-emerald-800 font-bold">{isPublic ? '****** ₫' : formatVndCurrency(stats.incoming)}</strong>
        </div>
      </div>

      <CounterCheckout />

      <div id="transactions-history" className="space-y-6 scroll-mt-6 pt-2">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h2 className="text-xl font-bold tracking-tight text-stone-900">Lịch sử giao dịch</h2>
          <p className="text-sm text-stone-600 mt-0.5">
            SePay Store, payOS và lịch sử ngân hàng đã lưu
          </p>
        </div>
        <div className="flex items-center gap-2 self-start sm:self-auto">
          <button
            type="button"
            onClick={() => refetch()}
            disabled={isLoading || isRefetching}
            title="Chỉ tải lại dữ liệu đã lưu trên máy chủ"
            className="inline-flex items-center gap-2 px-3 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-200 shadow-2xs hover:bg-stone-50 text-stone-700 transition disabled:opacity-50 cursor-pointer"
          >
            <RefreshCw className={`w-3.5 h-3.5 ${isRefetching ? 'animate-spin' : ''}`} />
            Tải lại dữ liệu đã lưu
          </button>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-x-5 gap-y-2 rounded-xl border border-stone-200 bg-white px-4 py-3 text-sm">
        <span>{dateRange === 'today' ? 'Giao dịch hôm nay' : 'Tổng số giao dịch'}: <strong>{stats.totalCount.toLocaleString('vi-VN')}</strong></span>
        <span>Tiền vào: <strong className="text-emerald-800">{isPublic ? '****** ₫' : formatVndCurrency(stats.incoming)}</strong></span>
        {!isPublic && <span>Tiền ra: <strong className="text-rose-800">{formatVndCurrency(stats.outgoing)}</strong></span>}
      </div>

      {/* Filter & Search Bar */}
      <div className="bg-white p-4 rounded-2xl border border-stone-200/80 shadow-xs space-y-3">
        <div className="flex flex-col md:flex-row items-stretch md:items-center justify-between gap-3">
          {/* Search input */}
          <div className="relative flex-1">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-stone-600" />
            <input
              type="text"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Tìm theo nội dung chuyển khoản, số tiền, mã giao dịch..."
              className="w-full pl-9 pr-4 py-2 bg-stone-50 border border-stone-200 rounded-xl text-xs text-stone-800 placeholder-stone-600 focus:outline-none focus:ring-2 focus:ring-stone-900/10 focus:border-stone-900 transition"
            />
          </div>

          {/* Date range filter buttons */}
          <div className="flex items-center gap-1 bg-stone-100 p-1 rounded-xl self-start md:self-auto">
            <button
              type="button"
              onClick={() => handleDateRangeChange('today')}
              className={`px-3 py-1.5 rounded-lg text-xs font-medium transition cursor-pointer ${
                dateRange === 'today' ? 'bg-white text-stone-900 shadow-2xs font-semibold' : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              Hôm nay
            </button>
            <button
              type="button"
              onClick={() => handleDateRangeChange('7days')}
              className={`px-3 py-1.5 rounded-lg text-xs font-medium transition cursor-pointer ${
                dateRange === '7days' ? 'bg-white text-stone-900 shadow-2xs font-semibold' : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              7 ngày
            </button>
            <button
              type="button"
              onClick={() => handleDateRangeChange('all')}
              className={`px-3 py-1.5 rounded-lg text-xs font-medium transition cursor-pointer ${
                dateRange === 'all' ? 'bg-white text-stone-900 shadow-2xs font-semibold' : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              Tất cả
            </button>
            <button
              type="button"
              onClick={() => handleDateRangeChange('custom')}
              className={`px-3 py-1.5 rounded-lg text-xs font-medium transition cursor-pointer flex items-center gap-1 ${
                dateRange === 'custom' ? 'bg-white text-stone-900 shadow-2xs font-semibold' : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              <Calendar className="w-3 h-3" />
              Tùy chọn
            </button>
          </div>
        </div>

        {/* Custom date range picker if 'custom' is active */}
        {dateRange === 'custom' && (
          <div className="flex flex-wrap items-center gap-2 pt-2 border-t border-stone-100 text-xs">
            <span className="text-stone-600">Từ ngày:</span>
            <input
              type="date"
              value={customFrom}
              onChange={(e) => {
                setCustomFrom(e.target.value);
                pagination.reset();
              }}
              className="px-2.5 py-1 bg-stone-50 border border-stone-200 rounded-lg text-xs text-stone-800"
            />
            <span className="text-stone-600">Đến ngày:</span>
            <input
              type="date"
              value={customTo}
              onChange={(e) => {
                setCustomTo(e.target.value);
                pagination.reset();
              }}
              className="px-2.5 py-1 bg-stone-50 border border-stone-200 rounded-lg text-xs text-stone-800"
            />
          </div>
        )}

        {/* Direction Filter */}
        {!isPublic && <div className="flex items-center gap-1.5 w-full sm:w-auto overflow-x-auto pb-1 sm:pb-0 pt-1">
          <button
            type="button"
            onClick={() => handleFilterTypeChange('all')}
            className={`px-3 py-1.5 rounded-xl text-xs font-medium transition cursor-pointer whitespace-nowrap ${
              filterType === 'all'
                ? 'bg-stone-900 text-white shadow-2xs'
                : 'bg-stone-100 text-stone-600 hover:bg-stone-200/70'
            }`}
          >
            Tất cả
          </button>
          <button
            type="button"
            onClick={() => handleFilterTypeChange('credit')}
            className={`px-3 py-1.5 rounded-xl text-xs font-medium transition cursor-pointer whitespace-nowrap ${
              filterType === 'credit'
                ? 'bg-emerald-600 text-white shadow-2xs'
                : 'bg-stone-100 text-stone-600 hover:bg-stone-200/70'
            }`}
          >
            Tiền vào
          </button>
          <button
            type="button"
            onClick={() => handleFilterTypeChange('debit')}
            className={`px-3 py-1.5 rounded-xl text-xs font-medium transition cursor-pointer whitespace-nowrap ${
              filterType === 'debit'
                ? 'bg-rose-600 text-white shadow-2xs'
                : 'bg-stone-100 text-stone-600 hover:bg-stone-200/70'
            }`}
          >
            Tiền ra
          </button>
        </div>}
      </div>

      {/* Transaction List */}
      <div className="bg-white rounded-2xl border border-stone-200/80 shadow-xs overflow-hidden">
        {isLoading ? (
          <div className="py-16 text-center text-xs text-stone-600">
            <RefreshCw className="w-6 h-6 animate-spin mx-auto mb-2 text-stone-600" />
            Đang tải dữ liệu giao dịch...
          </div>
        ) : isError ? (
          <div className="py-16 text-center text-xs text-rose-600 space-y-2">
            <p className="font-semibold">Không thể tải dữ liệu giao dịch: {error instanceof Error ? error.message : 'Lỗi kết nối'}</p>
            <button
              type="button"
              onClick={() => refetch()}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-semibold bg-white border border-rose-200 text-rose-700 hover:bg-rose-50 transition cursor-pointer shadow-2xs"
            >
              <RefreshCw className="w-3.5 h-3.5" />
              <span>Thử lại</span>
            </button>
          </div>
        ) : transactions.length === 0 ? (
          <div className="py-16 text-center text-xs text-stone-600 space-y-2">
            <Receipt className="w-8 h-8 mx-auto text-stone-600" />
            <p className="font-semibold text-stone-700">Không tìm thấy giao dịch nào</p>
            <p className="text-stone-600 max-w-sm mx-auto">
              Không có giao dịch nào khớp với bộ lọc hoặc ngân hàng chưa ghi nhận biến động mới trong khoảng thời gian này.
            </p>
          </div>
        ) : (
          <div className="divide-y divide-stone-100">
            {transactions.map((tx) => {
              const isCredit = tx.credit > 0;
              const displayDate = formatDateTimeVN(tx.transactionDate || tx.transactionDay || tx.firstSeenAt);
              return (
                <div
                  key={tx.id}
                  onClick={() => setSelectedTx(tx)}
                  className="p-4 hover:bg-stone-50/80 transition flex items-center justify-between gap-4 cursor-pointer group"
                >
                  <div className="flex items-center gap-3.5 min-w-0">
                    <div
                      className={`w-10 h-10 rounded-xl flex items-center justify-center shrink-0 ${
                        isCredit
                          ? 'bg-emerald-50 text-emerald-600 border border-emerald-100'
                          : 'bg-rose-50 text-rose-600 border border-rose-100'
                      }`}
                    >
                      {isCredit ? (
                        <ArrowDownLeft className="w-5 h-5" />
                      ) : (
                        <ArrowUpRight className="w-5 h-5" />
                      )}
                    </div>
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-2">
                        <span
                          className={`text-xs font-semibold px-2 py-0.5 rounded-md ${
                            isCredit
                              ? 'bg-emerald-50 text-emerald-700'
                              : 'bg-rose-50 text-rose-700'
                          }`}
                        >
                          {isCredit ? 'Tiền vào' : 'Tiền ra'}
                        </span>
                        <span className="text-xs font-medium text-stone-700">{tx.bank || 'Chưa rõ ngân hàng'}</span>
                        {tx.provider && <span className="text-xs font-semibold text-stone-700">{tx.provider === 'SEPAY' ? 'SePay · QR cửa hàng' : 'payOS'}</span>}
                        <span className="text-xs text-stone-600 flex items-center gap-1">
                          <Clock className="w-3 h-3" />
                          {displayDate}
                        </span>
                      </div>
                      <p className="text-sm font-medium text-stone-900 mt-1 truncate max-w-md sm:max-w-xl">
                        {tx.description || 'Không có nội dung chuyển khoản'}
                      </p>
                    </div>
                  </div>

                  <div className="flex items-center gap-3 shrink-0 text-right">
                    <div>
                      <span
                        className={`text-base font-bold block ${
                          isCredit ? 'text-emerald-600' : 'text-rose-600'
                        }`}
                      >
                        {isCredit ? '+' : '-'}
                        {formatVndCurrency(isCredit ? tx.credit : tx.debit)}
                      </span>
                      {tx.balance !== undefined && tx.balance !== null && (
                        <span className="text-xs text-stone-600 font-mono">
                          Số dư: {isPublic ? '****** ₫' : formatVndCurrency(tx.balance)}
                        </span>
                      )}
                    </div>
                    <ChevronRight className="w-4 h-4 text-stone-600 group-hover:text-stone-600 transition" />
                  </div>
                </div>
              );
            })}
          </div>
        )}

        {/* Pagination Controls */}
        {(transactions.length > 0 || pagination.hasPrev) && (
          <PaginationControls
            pageNumber={pagination.pageNumber}
            itemCount={transactions.length}
            pageSize={pagination.pageSize}
            hasNext={Boolean(data?.nextCursor)}
            hasPrev={pagination.hasPrev}
            isLoading={isLoading}
            onNext={() => pagination.handleNext(data?.nextCursor)}
            onPrev={pagination.handlePrev}
            onFirst={pagination.handleFirst}
            onPageSizeChange={pagination.setPageSize}
          />
        )}
      </div>
      </div>

      {/* Transaction Detail Modal */}
      {selectedTx && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4 bg-stone-900/40 backdrop-blur-xs animate-in fade-in duration-150">
          <div className="bg-white w-full max-w-md rounded-2xl shadow-xl border border-stone-200 overflow-hidden">
            {/* Modal Header */}
            <div className="px-5 py-4 border-b border-stone-100 flex items-center justify-between">
              <div className="flex items-center gap-2">
                <Receipt className="w-4 h-4 text-stone-600" />
                <h3 className="font-bold text-stone-900 text-sm">Chi tiết giao dịch</h3>
              </div>
              <button
                type="button"
                onClick={() => setSelectedTx(null)}
                className="p-1 text-stone-600 hover:text-stone-600 rounded-lg hover:bg-stone-100 transition cursor-pointer"
              >
                <X className="w-4 h-4" />
              </button>
            </div>

            {/* Modal Body */}
            <div className="p-5 space-y-4 text-xs">
              <div className="text-center py-2">
                <span
                  className={`text-2xl font-bold tracking-tight block ${
                    selectedTx.credit > 0 ? 'text-emerald-600' : 'text-rose-600'
                  }`}
                >
                  {selectedTx.credit > 0 ? '+' : '-'}
                  {formatVndCurrency(
                    selectedTx.credit > 0 ? selectedTx.credit : selectedTx.debit
                  )}
                </span>
                <span className="text-stone-600 font-medium mt-1 inline-block">
                  {selectedTx.credit > 0 ? 'Giao dịch nhận tiền' : 'Giao dịch chuyển tiền'}
                </span>
              </div>

              <div className="bg-stone-50 rounded-xl p-3 space-y-2 border border-stone-100">
                <div className="flex justify-between py-1 border-b border-stone-200/50">
                  <span className="text-stone-600 font-medium">Ngân hàng</span>
                  <span className="font-semibold text-stone-800">{selectedTx.bank || 'Chưa rõ ngân hàng'}</span>
                </div>
                {selectedTx.orderCode && <div className="flex justify-between py-1"><span>Mã đơn payOS</span><span className="font-mono">{selectedTx.orderCode}</span></div>}
                <div className="flex justify-between py-1 border-b border-stone-200/50">
                  <span className="text-stone-600 font-medium">Mã giao dịch</span>
                  <div className="flex items-center gap-1.5">
                    <span className="font-mono font-semibold text-stone-800">
                      {selectedTx.semanticKey}
                    </span>
                    <button
                      type="button"
                      onClick={() => handleCopy(selectedTx.semanticKey)}
                      className="text-stone-600 hover:text-stone-700 transition cursor-pointer"
                    >
                      {copiedId ? (
                        <Check className="w-3.5 h-3.5 text-emerald-600" />
                      ) : (
                        <Copy className="w-3.5 h-3.5" />
                      )}
                    </button>
                  </div>
                </div>

                <div className="flex justify-between py-1 border-b border-stone-200/50">
                  <span className="text-stone-600 font-medium">Thời gian giao dịch</span>
                  <span className="font-semibold text-stone-800">
                    {formatDateTimeVN(selectedTx.transactionDate || selectedTx.transactionDay || selectedTx.firstSeenAt)}
                  </span>
                </div>

                {selectedTx.balance !== undefined && selectedTx.balance !== null && (
                  <div className="flex justify-between py-1 border-b border-stone-200/50">
                    <span className="text-stone-600 font-medium">Số dư sau giao dịch</span>
                    <span className="font-mono font-semibold text-stone-800">
                      {isPublic ? '****** ₫' : formatVndCurrency(selectedTx.balance)}
                    </span>
                  </div>
                )}

                <div className="flex justify-between py-1">
                  <span className="text-stone-600 font-medium">Nguồn thu thập</span>
                  <span className="font-mono font-semibold text-stone-800">
                    {selectedTx.source || 'REALTIME'}
                  </span>
                </div>
              </div>

              <div>
                <span className="text-stone-600 font-medium block mb-1">
                  Nội dung chuyển khoản
                </span>
                <div className="p-3 bg-stone-50 rounded-xl border border-stone-100 font-medium text-stone-800 break-words">
                  {selectedTx.description || 'Không có nội dung'}
                </div>
              </div>
            </div>

            {/* Modal Footer */}
            <div className="px-5 py-3 bg-stone-50 border-t border-stone-100 flex items-center justify-between">
              <a
                href={ROUTES.transactionDetail(selectedTx.id)}
                className="text-stone-600 hover:text-stone-900 font-semibold text-xs"
              >
                Mở trang riêng &rarr;
              </a>
              <button
                type="button"
                onClick={() => setSelectedTx(null)}
                className="px-4 py-2 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 transition cursor-pointer"
              >
                Đóng
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};
