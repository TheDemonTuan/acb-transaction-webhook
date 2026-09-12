import React, { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import {
  Search,
  ArrowDownLeft,
  ArrowUpRight,
  RefreshCw,
  Clock,
  ChevronRight,
  TrendingUp,
  TrendingDown,
  Receipt,
  Copy,
  Check,
  X,
  Calendar,
} from 'lucide-react';
import { fetchTransactions, ensureHistory } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { formatVndCurrency } from '../../shared/formatters/money';
import type { Transaction } from '../../realtime-types';

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
  const [filterType, setFilterType] = useState<'all' | 'credit' | 'debit'>('all');
  const [dateRange, setDateRange] = useState<'today' | '7days' | 'all' | 'custom'>('all');
  const [customFrom, setCustomFrom] = useState('');
  const [customTo, setCustomTo] = useState('');
  const [selectedTx, setSelectedTx] = useState<Transaction | null>(null);
  const [copiedId, setCopiedId] = useState(false);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [syncingHistory, setSyncingHistory] = useState(false);
  const [syncNotice, setSyncNotice] = useState<{ kind: 'ok' | 'error'; text: string } | null>(null);

  const handleSyncHistory = async () => {
    if (!queryParams.from || !queryParams.to) return;

    setSyncingHistory(true);
    setSyncNotice(null);
    try {
      const result = await ensureHistory({ from: queryParams.from, to: queryParams.to });
      await refetch();
      setSyncNotice({
        kind: 'ok',
        text: result.rowsSeen
          ? `Đã đồng bộ ${result.rowsSeen} giao dịch từ ACB.`
          : 'Đã kiểm tra ACB. Không có giao dịch trong khoảng ngày đã chọn.',
      });
    } catch (err) {
      setSyncNotice({
        kind: 'error',
        text: err instanceof Error ? err.message : 'Không thể đồng bộ giao dịch từ ACB.',
      });
    } finally {
      setSyncingHistory(false);
    }
  };

  // Debounce search input
  useEffect(() => {
    const timer = setTimeout(() => {
      setDebouncedSearch(search);
      setCursor(undefined); // Reset cursor on new search
    }, 300);
    return () => clearTimeout(timer);
  }, [search]);

  // Reset cursor when filters change
  const handleDateRangeChange = (range: 'today' | '7days' | 'all' | 'custom') => {
    setDateRange(range);
    setCursor(undefined);
  };

  const handleFilterTypeChange = (type: 'all' | 'credit' | 'debit') => {
    setFilterType(type);
    setCursor(undefined);
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
      direction: filterType,
      query: debouncedSearch.trim() || undefined,
      limit: 50,
      cursor,
    };
  }, [dateRange, customFrom, customTo, filterType, debouncedSearch, cursor]);

  const { data, isLoading, isRefetching, refetch } = useQuery({
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
    <div className="space-y-6">
      {/* Top section with heading & quick actions */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h2 className="text-2xl font-bold tracking-tight text-stone-900">Giao dịch</h2>
          <p className="text-sm text-stone-600 mt-0.5">
            Danh sách giao dịch ngân hàng ACB được đồng bộ và thống kê trực tiếp từ máy chủ
          </p>
        </div>
        <div className="flex items-center gap-2 self-start sm:self-auto">
          <button
            type="button"
            onClick={handleSyncHistory}
            disabled={syncingHistory || isLoading || isRefetching || !queryParams.from || !queryParams.to}
            title={dateRange === 'all' ? 'Chọn Hôm nay, 7 ngày hoặc một khoảng ngày để đồng bộ từ ACB' : undefined}
            className="inline-flex items-center gap-1.5 px-3 py-2 rounded-xl text-xs font-semibold bg-stone-900 text-white shadow-2xs hover:bg-stone-800 transition disabled:opacity-50 cursor-pointer disabled:cursor-not-allowed"
          >
            <RefreshCw className={`w-3.5 h-3.5 ${syncingHistory ? 'animate-spin' : ''}`} />
            {syncingHistory ? 'Đang đồng bộ ACB...' : 'Đồng bộ từ ACB'}
          </button>
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

      {syncNotice && (
        <div
          role={syncNotice.kind === 'error' ? 'alert' : 'status'}
          className={`rounded-xl border px-4 py-3 text-sm ${
            syncNotice.kind === 'error'
              ? 'border-red-200 bg-red-50 text-red-700'
              : 'border-emerald-200 bg-emerald-50 text-emerald-700'
          }`}
        >
          {syncNotice.text}
        </div>
      )}

      {/* KPI Stats Grid - Server Aggregate */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
        {/* Total count */}
        <div className="bg-white p-5 rounded-2xl border border-stone-200/80 shadow-xs flex items-center gap-4">
          <div className="w-12 h-12 rounded-xl bg-stone-100 flex items-center justify-center text-stone-600 shrink-0">
            <Receipt className="w-6 h-6" />
          </div>
          <div className="min-w-0">
            <p className="text-xs font-medium text-stone-600">
              {dateRange === 'today' ? 'Giao dịch hôm nay' : 'Tổng số giao dịch'}
            </p>
            <p className="text-2xl font-bold text-stone-900 tracking-tight mt-0.5">
              {stats.totalCount.toLocaleString('vi-VN')}
            </p>
          </div>
        </div>

        {/* Incoming */}
        <div className="bg-white p-5 rounded-2xl border border-stone-200/80 shadow-xs flex items-center gap-4">
          <div className="w-12 h-12 rounded-xl bg-emerald-50 flex items-center justify-center text-emerald-600 shrink-0">
            <TrendingUp className="w-6 h-6" />
          </div>
          <div className="min-w-0">
            <p className="text-xs font-medium text-stone-600">
              {dateRange === 'today' ? 'Tiền vào hôm nay' : 'Tổng tiền vào'}
            </p>
            <p className="text-2xl font-bold text-emerald-600 tracking-tight mt-0.5 truncate">
              {formatVndCurrency(stats.incoming)}
            </p>
          </div>
        </div>

        {/* Outgoing */}
        <div className="bg-white p-5 rounded-2xl border border-stone-200/80 shadow-xs flex items-center gap-4">
          <div className="w-12 h-12 rounded-xl bg-rose-50 flex items-center justify-center text-rose-600 shrink-0">
            <TrendingDown className="w-6 h-6" />
          </div>
          <div className="min-w-0">
            <p className="text-xs font-medium text-stone-600">
              {dateRange === 'today' ? 'Tiền ra hôm nay' : 'Tổng tiền ra'}
            </p>
            <p className="text-2xl font-bold text-rose-600 tracking-tight mt-0.5 truncate">
              {formatVndCurrency(stats.outgoing)}
            </p>
          </div>
        </div>
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
                setCursor(undefined);
              }}
              className="px-2.5 py-1 bg-stone-50 border border-stone-200 rounded-lg text-xs text-stone-800"
            />
            <span className="text-stone-600">Đến ngày:</span>
            <input
              type="date"
              value={customTo}
              onChange={(e) => {
                setCustomTo(e.target.value);
                setCursor(undefined);
              }}
              className="px-2.5 py-1 bg-stone-50 border border-stone-200 rounded-lg text-xs text-stone-800"
            />
          </div>
        )}

        {/* Direction Filter */}
        <div className="flex items-center gap-1.5 w-full sm:w-auto overflow-x-auto pb-1 sm:pb-0 pt-1">
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
        </div>
      </div>

      {/* Transaction List */}
      <div className="bg-white rounded-2xl border border-stone-200/80 shadow-xs overflow-hidden">
        {isLoading ? (
          <div className="py-16 text-center text-xs text-stone-600">
            <RefreshCw className="w-6 h-6 animate-spin mx-auto mb-2 text-stone-600" />
            Đang tải dữ liệu giao dịch...
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
              const displayDate = tx.transactionDay || tx.transactionDate || tx.firstSeenAt;

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
                      <div className="flex items-center gap-2">
                        <span
                          className={`text-xs font-semibold px-2 py-0.5 rounded-md ${
                            isCredit
                              ? 'bg-emerald-50 text-emerald-700'
                              : 'bg-rose-50 text-rose-700'
                          }`}
                        >
                          {isCredit ? 'Tiền vào' : 'Tiền ra'}
                        </span>
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
                          Số dư: {formatVndCurrency(tx.balance)}
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

        {/* Pagination: Load More */}
        {data?.nextCursor && (
          <div className="p-4 border-t border-stone-100 text-center bg-stone-50/50">
            <button
              type="button"
              onClick={() => setCursor(data.nextCursor)}
              disabled={isLoading || isRefetching}
              className="px-4 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-100 shadow-2xs transition cursor-pointer disabled:opacity-50"
            >
              Tải trang tiếp theo ({stats.totalCount - transactions.length} giao dịch còn lại)
            </button>
          </div>
        )}
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
                    {selectedTx.transactionDate || selectedTx.transactionDay || selectedTx.firstSeenAt}
                  </span>
                </div>

                {selectedTx.balance !== undefined && selectedTx.balance !== null && (
                  <div className="flex justify-between py-1 border-b border-stone-200/50">
                    <span className="text-stone-600 font-medium">Số dư sau giao dịch</span>
                    <span className="font-mono font-semibold text-stone-800">
                      {formatVndCurrency(selectedTx.balance)}
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
                href={`/transactions/${selectedTx.id}`}
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
