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



export const TransactionHistoryView: React.FC = () => {
  const [search, setSearch] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const isPublic = isPublicViewerHost();
  const [filterType, setFilterType] = useState<'all' | 'credit' | 'debit'>(isPublic ? 'credit' : 'all');
  const [dateRange, setDateRange] = useState<'today' | '7days' | '30days' | 'all' | 'custom'>('today');
  const [customFrom, setCustomFrom] = useState('');
  const [customTo, setCustomTo] = useState('');
  const [selectedTx, setSelectedTx] = useState<Transaction | null>(null);
  const [copiedId, setCopiedId] = useState(false);
  const pagination = useCursorPagination(20);
  const resetPagination = pagination.reset;
  const appliedSearchRef = useRef('');

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

  const handleDateRangeChange = (range: 'today' | '7days' | '30days' | 'all' | 'custom') => {
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
    const today = new Date().toISOString().slice(0, 10);
    if (dateRange === 'today') {
      from = today;
      to = today;
    } else if (dateRange === '7days') {
      from = new Date(Date.now() - 6 * 86_400_000).toISOString().slice(0, 10);
      to = today;
    } else if (dateRange === '30days') {
      from = new Date(Date.now() - 29 * 86_400_000).toISOString().slice(0, 10);
      to = today;
    } else if (dateRange === 'custom') {
      from = customFrom || undefined;
      to = customTo || undefined;
    }
    return {
      from,
      to,
      direction: isPublic ? ('credit' as const) : filterType,
      query: debouncedSearch.trim() || undefined,
      limit: pagination.pageSize,
      cursor: pagination.cursor,
    };
  }, [
    isPublic,
    dateRange,
    customFrom,
    customTo,
    filterType,
    debouncedSearch,
    pagination.pageSize,
    pagination.cursor,
  ]);

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
      void navigator.clipboard.writeText(text);
      setCopiedId(true);
      setTimeout(() => setCopiedId(false), 2000);
    }
  };

  return (
    <div id="transactions-history" className="space-y-4 min-w-0">
      {/* Header & Refresh */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3 bg-white p-4 rounded-2xl border border-stone-200/80 shadow-xs">
        <div>
          <h2 className="text-lg sm:text-xl font-black tracking-tight text-stone-900">
            Lịch sử giao dịch
          </h2>
          <p className="text-xs text-stone-500 mt-0.5">
            Tổng hợp dữ liệu ngân hàng, SePay Store và payOS
          </p>
        </div>
        <div className="flex items-center gap-2 self-start sm:self-auto">
          <button
            type="button"
            onClick={() => void refetch()}
            disabled={isLoading || isRefetching}
            className="inline-flex items-center gap-2 px-3 py-2 rounded-xl text-xs font-semibold bg-stone-50 border border-stone-200 hover:bg-stone-100 text-stone-700 transition disabled:opacity-50 cursor-pointer shadow-2xs"
          >
            <RefreshCw className={`w-3.5 h-3.5 ${isRefetching ? 'animate-spin' : ''}`} />
            <span>Tải lại dữ liệu đã lưu</span>
          </button>
        </div>
      </div>

      {/* Stats Summary Bar */}
      <div className="grid grid-cols-2 sm:grid-cols-3 gap-3">
        <div className="bg-white p-3.5 rounded-2xl border border-stone-200/80 shadow-xs">
          <span className="text-[11px] font-semibold text-stone-500 uppercase tracking-wider block">
            {dateRange === 'today' ? 'Số đơn hôm nay' : 'Tổng giao dịch'}
          </span>
          <p className="text-xl sm:text-2xl font-black text-stone-900 mt-0.5">
            {stats.totalCount.toLocaleString('vi-VN')}
          </p>
        </div>

        <div className="bg-white p-3.5 rounded-2xl border border-stone-200/80 shadow-xs">
          <span className="text-[11px] font-semibold text-emerald-700 uppercase tracking-wider block">
            Tổng tiền vào
          </span>
          <p className="text-xl sm:text-2xl font-black text-emerald-700 mt-0.5 truncate">
            {isPublic ? '****** ₫' : formatVndCurrency(stats.incoming)}
          </p>
        </div>

        {!isPublic && (
          <div className="bg-white p-3.5 rounded-2xl border border-stone-200/80 shadow-xs col-span-2 sm:col-span-1">
            <span className="text-[11px] font-semibold text-rose-700 uppercase tracking-wider block">
              Tổng tiền ra
            </span>
            <p className="text-xl sm:text-2xl font-black text-rose-700 mt-0.5 truncate">
              {formatVndCurrency(stats.outgoing)}
            </p>
          </div>
        )}
      </div>

      {/* Filter & Search Bar */}
      <div className="bg-white p-4 rounded-2xl border border-stone-200/80 shadow-xs space-y-3">
        <div className="flex flex-col md:flex-row items-stretch md:items-center justify-between gap-3">
          {/* Search input */}
          <div className="relative flex-1">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-stone-400" />
            <input
              type="text"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Tìm theo nội dung chuyển khoản, số tiền, mã giao dịch..."
              className="w-full pl-9 pr-4 py-2.5 bg-stone-50 border border-stone-200 rounded-xl text-xs text-stone-800 placeholder-stone-400 focus:outline-none focus:ring-2 focus:ring-stone-900/10 focus:border-stone-900 transition"
            />
          </div>

          {/* Date range filter buttons */}
          <div className="flex flex-wrap items-center gap-1 bg-stone-100 p-1 rounded-xl self-start md:self-auto text-xs">
            <button
              type="button"
              onClick={() => handleDateRangeChange('today')}
              className={`px-3 py-1.5 rounded-lg font-semibold transition cursor-pointer ${
                dateRange === 'today'
                  ? 'bg-white text-stone-900 shadow-2xs'
                  : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              Hôm nay
            </button>
            <button
              type="button"
              onClick={() => handleDateRangeChange('7days')}
              className={`px-3 py-1.5 rounded-lg font-semibold transition cursor-pointer ${
                dateRange === '7days'
                  ? 'bg-white text-stone-900 shadow-2xs'
                  : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              7 ngày
            </button>
            <button
              type="button"
              onClick={() => handleDateRangeChange('30days')}
              className={`px-3 py-1.5 rounded-lg font-semibold transition cursor-pointer ${
                dateRange === '30days'
                  ? 'bg-white text-stone-900 shadow-2xs'
                  : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              30 ngày
            </button>
            <button
              type="button"
              onClick={() => handleDateRangeChange('all')}
              className={`px-3 py-1.5 rounded-lg font-semibold transition cursor-pointer ${
                dateRange === 'all'
                  ? 'bg-white text-stone-900 shadow-2xs'
                  : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              Tất cả
            </button>
            <button
              type="button"
              onClick={() => handleDateRangeChange('custom')}
              className={`px-3 py-1.5 rounded-lg font-semibold transition cursor-pointer flex items-center gap-1 ${
                dateRange === 'custom'
                  ? 'bg-white text-stone-900 shadow-2xs'
                  : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              <Calendar className="w-3 h-3" />
              <span>Tùy chọn</span>
            </button>
          </div>
        </div>

        {/* Custom date range picker */}
        {dateRange === 'custom' && (
          <div className="flex flex-wrap items-center gap-2 pt-2 border-t border-stone-100 text-xs">
            <span className="text-stone-600 font-medium">Từ ngày:</span>
            <input
              type="date"
              value={customFrom}
              onChange={(e) => {
                setCustomFrom(e.target.value);
                pagination.reset();
              }}
              className="px-2.5 py-1 bg-stone-50 border border-stone-200 rounded-lg text-xs text-stone-800"
            />
            <span className="text-stone-600 font-medium">Đến ngày:</span>
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
        {!isPublic && (
          <div className="flex items-center gap-1.5 pt-1 border-t border-stone-100/70 overflow-x-auto">
            <span className="text-xs text-stone-500 mr-1">Chiều tiền:</span>
            <button
              type="button"
              onClick={() => handleFilterTypeChange('all')}
              className={`px-3 py-1 rounded-lg text-xs font-semibold transition cursor-pointer ${
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
              className={`px-3 py-1 rounded-lg text-xs font-semibold transition cursor-pointer ${
                filterType === 'credit'
                  ? 'bg-emerald-700 text-white shadow-2xs'
                  : 'bg-stone-100 text-stone-600 hover:bg-stone-200/70'
              }`}
            >
              Tiền vào
            </button>
            <button
              type="button"
              onClick={() => handleFilterTypeChange('debit')}
              className={`px-3 py-1 rounded-lg text-xs font-semibold transition cursor-pointer ${
                filterType === 'debit'
                  ? 'bg-rose-700 text-white shadow-2xs'
                  : 'bg-stone-100 text-stone-600 hover:bg-stone-200/70'
              }`}
            >
              Tiền ra
            </button>
          </div>
        )}
      </div>

      {/* Transaction List */}
      <div className="bg-white rounded-2xl border border-stone-200/80 shadow-xs overflow-hidden">
        {isLoading ? (
          <div className="py-20 text-center text-xs text-stone-600">
            <RefreshCw className="w-6 h-6 animate-spin mx-auto mb-2 text-stone-400" />
            Đang tải dữ liệu giao dịch...
          </div>
        ) : isError ? (
          <div className="py-16 text-center text-xs text-rose-600 space-y-2">
            <p className="font-semibold">
              Không thể tải dữ liệu: {error instanceof Error ? error.message : 'Lỗi kết nối'}
            </p>
            <button
              type="button"
              onClick={() => void refetch()}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-semibold bg-white border border-rose-200 text-rose-700 hover:bg-rose-50 transition cursor-pointer shadow-2xs"
            >
              <RefreshCw className="w-3.5 h-3.5" />
              <span>Thử lại</span>
            </button>
          </div>
        ) : transactions.length === 0 ? (
          <div className="py-20 text-center text-xs text-stone-500 space-y-2">
            <Receipt className="w-8 h-8 mx-auto text-stone-400" />
            <p className="font-bold text-stone-700 text-sm">Không tìm thấy giao dịch nào</p>
            <p className="text-stone-500 max-w-sm mx-auto">
              Không có giao dịch nào khớp với bộ lọc hoặc ngân hàng chưa ghi nhận biến động mới trong khoảng thời gian này.
            </p>
          </div>
        ) : (
          <div className="divide-y divide-stone-100">
            {transactions.map((tx) => {
              const isCredit = tx.credit > 0;
              const displayDate = formatDateTimeVN(
                tx.transactionDate || tx.transactionDay || tx.firstSeenAt
              );
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
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span
                          className={`text-[11px] font-bold px-2 py-0.5 rounded-md ${
                            isCredit
                              ? 'bg-emerald-50 text-emerald-700 border border-emerald-200'
                              : 'bg-rose-50 text-rose-700 border border-rose-200'
                          }`}
                        >
                          {isCredit ? 'Tiền vào' : 'Tiền ra'}
                        </span>
                        <span className="text-xs font-semibold text-stone-800">
                          {tx.bank || 'ACB'}
                        </span>
                        {tx.provider && (
                          <span
                            className={`text-[10px] font-bold px-1.5 py-0.5 rounded ${
                              tx.provider === 'SEPAY'
                                ? 'bg-emerald-100 text-emerald-800'
                                : 'bg-blue-100 text-blue-800'
                            }`}
                          >
                            {tx.provider === 'SEPAY' ? 'SePay Store' : 'payOS'}
                          </span>
                        )}
                        <span className="text-[11px] text-stone-500 flex items-center gap-1">
                          <Clock className="w-3 h-3 text-stone-400" />
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
                        className={`text-base font-black block tracking-tight ${
                          isCredit ? 'text-emerald-700' : 'text-rose-700'
                        }`}
                      >
                        {isCredit ? '+' : '-'}
                        {formatVndCurrency(isCredit ? tx.credit : tx.debit)}
                      </span>
                      {tx.balance !== undefined && tx.balance !== null && (
                        <span className="text-[11px] text-stone-500 font-mono">
                          Số dư: {isPublic ? '****** ₫' : formatVndCurrency(tx.balance)}
                        </span>
                      )}
                    </div>
                    <ChevronRight className="w-4 h-4 text-stone-400 group-hover:text-stone-700 transition" />
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

      {/* Transaction Detail Modal */}
      {selectedTx && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4 bg-stone-950/70 backdrop-blur-xs animate-in fade-in">
          <div className="bg-white w-full max-w-md rounded-3xl shadow-2xl border border-stone-200 overflow-hidden">
            {/* Modal Header */}
            <div className="px-5 py-4 border-b border-stone-100 flex items-center justify-between">
              <div className="flex items-center gap-2">
                <Receipt className="w-4 h-4 text-stone-600" />
                <h3 className="font-bold text-stone-900 text-sm">Chi tiết giao dịch</h3>
              </div>
              <button
                type="button"
                onClick={() => setSelectedTx(null)}
                className="p-1 text-stone-400 hover:text-stone-700 rounded-lg hover:bg-stone-100 transition cursor-pointer"
              >
                <X className="w-4 h-4" />
              </button>
            </div>

            {/* Modal Body */}
            <div className="p-5 space-y-4 text-xs">
              <div className="text-center py-2">
                <span
                  className={`text-2xl font-black tracking-tight block ${
                    selectedTx.credit > 0 ? 'text-emerald-700' : 'text-rose-700'
                  }`}
                >
                  {selectedTx.credit > 0 ? '+' : '-'}
                  {formatVndCurrency(
                    selectedTx.credit > 0 ? selectedTx.credit : selectedTx.debit
                  )}
                </span>
                <span className="text-stone-500 font-semibold mt-1 inline-block">
                  {selectedTx.credit > 0 ? 'Giao dịch nhận tiền' : 'Giao dịch chuyển tiền'}
                </span>
              </div>

              <div className="bg-stone-50 rounded-2xl p-3.5 space-y-2 border border-stone-100">
                <div className="flex justify-between py-1 border-b border-stone-200/50">
                  <span className="text-stone-500 font-medium">Ngân hàng</span>
                  <span className="font-semibold text-stone-800">
                    {selectedTx.bank || 'Chưa rõ ngân hàng'}
                  </span>
                </div>
                {selectedTx.orderCode && (
                  <div className="flex justify-between py-1 border-b border-stone-200/50">
                    <span className="text-stone-500 font-medium">Mã đơn payOS</span>
                    <span className="font-mono font-bold">{selectedTx.orderCode}</span>
                  </div>
                )}
                <div className="flex justify-between py-1 border-b border-stone-200/50">
                  <span className="text-stone-500 font-medium">Mã giao dịch</span>
                  <div className="flex items-center gap-1.5">
                    <span className="font-mono font-bold text-stone-800">
                      {selectedTx.semanticKey}
                    </span>
                    <button
                      type="button"
                      onClick={() => handleCopy(selectedTx.semanticKey)}
                      className="text-stone-500 hover:text-stone-700 transition cursor-pointer"
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
                  <span className="text-stone-500 font-medium">Thời gian giao dịch</span>
                  <span className="font-semibold text-stone-800">
                    {formatDateTimeVN(
                      selectedTx.transactionDate || selectedTx.transactionDay || selectedTx.firstSeenAt
                    )}
                  </span>
                </div>

                {selectedTx.balance !== undefined && selectedTx.balance !== null && (
                  <div className="flex justify-between py-1 border-b border-stone-200/50">
                    <span className="text-stone-500 font-medium">Số dư sau giao dịch</span>
                    <span className="font-mono font-bold text-stone-800">
                      {isPublic ? '****** ₫' : formatVndCurrency(selectedTx.balance)}
                    </span>
                  </div>
                )}

                <div className="flex justify-between py-1">
                  <span className="text-stone-500 font-medium">Nguồn dữ liệu</span>
                  <span className="font-mono font-semibold text-stone-800">
                    {selectedTx.source || 'REALTIME'}
                  </span>
                </div>
              </div>

              <div>
                <span className="text-stone-500 font-medium block mb-1">
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
                className="text-emerald-700 hover:text-emerald-800 font-bold text-xs"
              >
                Mở trang riêng &rarr;
              </a>
              <button
                type="button"
                onClick={() => setSelectedTx(null)}
                className="px-4 py-2 rounded-xl text-xs font-bold bg-stone-900 text-white hover:bg-stone-800 transition cursor-pointer"
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
