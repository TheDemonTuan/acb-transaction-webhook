import React, { useState } from 'react';
import { useParams, Link, useNavigate } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import {
  ArrowLeft,
  ArrowDownLeft,
  ArrowUpRight,
  Clock,
  Copy,
  Check,
  Volume2,
  Calendar,
  Hash,
  Wallet,
  Receipt,
} from 'lucide-react';
import { fetchTransactionDetail } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { formatVndCurrency } from '../../shared/formatters/money';
import { useVoiceAnnouncements } from '../../features/voice-announcements/VoiceAnnouncementProvider';
import { buildSingleTransactionPhrase } from '../../features/voice-announcements/voice-copy';
import { isPublicViewerHost } from '../../app/runtime-mode';

export const TransactionDetailPage: React.FC = () => {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [copied, setCopied] = useState(false);
  const { replayVoice, testVoice, settings } = useVoiceAnnouncements();

  const { data: transaction, isLoading } = useQuery({
    queryKey: queryKeys.transactionDetail(id || ''),
    queryFn: () => fetchTransactionDetail(id || ''),
    enabled: !!id,
  });

  if (isLoading) {
    return (
      <div className="max-w-2xl mx-auto py-12 text-center text-stone-500">
        Đang tải thông tin chi tiết giao dịch...
      </div>
    );
  }

  if (!transaction) {
    return (
      <div className="max-w-2xl mx-auto py-12 text-center space-y-4">
        <Receipt className="w-12 h-12 text-stone-300 mx-auto" />
        <h3 className="text-lg font-bold text-stone-800">Không tìm thấy giao dịch</h3>
        <p className="text-xs text-stone-500">
          Giao dịch mang mã <span className="font-mono font-semibold">{id}</span> không tồn tại hoặc đã được lưu trữ ở trang cũ.
        </p>
        <Link
          to="/transactions"
          className="inline-flex items-center gap-2 px-4 py-2 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 transition"
        >
          <ArrowLeft className="w-4 h-4" />
          <span>Quay lại danh sách giao dịch</span>
        </Link>
      </div>
    );
  }

  const isCredit = transaction.credit > 0;
  const formattedDate = transaction.transactionDate
    ? new Date(transaction.transactionDate).toLocaleString('vi-VN', {
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        day: '2-digit',
        month: '2-digit',
        year: 'numeric',
      })
    : 'Chưa rõ thời gian';

  const handleCopy = () => {
    const summary = `${isCredit ? 'Tiền vào' : 'Tiền ra'}: ${formatVndCurrency(
      isCredit ? transaction.credit : transaction.debit
    )}\nThời gian: ${formattedDate}\nNội dung: ${transaction.description}\nMã giao dịch: ${
      transaction.semanticKey || transaction.id
    }`;
    navigator.clipboard.writeText(summary);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  const handleSpeak = () => {
    if (!isCredit || !transaction.id) return;

    if (isPublicViewerHost()) {
      const phrase = buildSingleTransactionPhrase(
        String(transaction.credit),
        transaction.description || '',
        {
          includeDescription: settings.includeDescription,
        },
      );

      void testVoice(phrase);
      return;
    }

    void replayVoice(transaction.id);
  };

  return (
    <div className="max-w-2xl mx-auto space-y-6">
      {/* Navigation header */}
      <div className="flex items-center justify-between">
        <button
          type="button"
          onClick={() => navigate('/transactions')}
          className="inline-flex items-center gap-2 text-xs font-semibold text-stone-600 hover:text-stone-900 transition cursor-pointer"
        >
          <ArrowLeft className="w-4 h-4" />
          <span>Quay lại danh sách</span>
        </button>
        <span className="text-xs text-stone-400 font-mono">ID: {transaction.id}</span>
      </div>

      {/* Main Detail Card */}
      <div className="bg-white rounded-3xl border border-stone-200 shadow-xs overflow-hidden">
        {/* Top visual hero */}
        <div
          className={`p-8 text-center border-b ${
            isCredit
              ? 'bg-gradient-to-b from-emerald-50/70 to-white border-emerald-100'
              : 'bg-gradient-to-b from-rose-50/70 to-white border-rose-100'
          }`}
        >
          <div
            className={`w-14 h-14 rounded-2xl mx-auto flex items-center justify-center mb-4 ${
              isCredit
                ? 'bg-emerald-100 text-emerald-700 border border-emerald-200'
                : 'bg-rose-100 text-rose-700 border border-rose-200'
            }`}
          >
            {isCredit ? <ArrowDownLeft className="w-7 h-7" /> : <ArrowUpRight className="w-7 h-7" />}
          </div>

          <span
            className={`inline-block text-xs font-bold px-3 py-1 rounded-full uppercase tracking-wider mb-2 ${
              isCredit
                ? 'bg-emerald-100 text-emerald-800'
                : 'bg-rose-100 text-rose-800'
            }`}
          >
            {isCredit ? 'Tiền vào tài khoản' : 'Tiền ra tài khoản'}
          </span>

          <h2
            className={`text-4xl font-black tracking-tight ${
              isCredit ? 'text-emerald-600' : 'text-rose-600'
            }`}
          >
            {isCredit ? `+${formatVndCurrency(transaction.credit)}` : `-${formatVndCurrency(transaction.debit)}`}
          </h2>

          <p className="text-xs text-stone-500 mt-2 flex items-center justify-center gap-1.5 font-medium">
            <Clock className="w-3.5 h-3.5" />
            <span>{formattedDate}</span>
          </p>
        </div>

        {/* Breakdown details */}
        <div className="p-6 sm:p-8 space-y-6">
          {/* Description */}
          <div>
            <span className="text-xs font-semibold text-stone-500 uppercase tracking-wider block mb-1.5">
              Nội dung chuyển khoản
            </span>
            <div className="p-4 rounded-2xl bg-stone-50 border border-stone-200/70 text-sm font-medium text-stone-900 leading-relaxed break-words">
              {transaction.description || '(Không có nội dung)'}
            </div>
          </div>

          {/* Grid attributes */}
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div className="p-4 rounded-2xl bg-stone-50/50 border border-stone-200/60 space-y-1">
              <span className="text-xs font-semibold text-stone-500 flex items-center gap-1.5">
                <Hash className="w-3.5 h-3.5" />
                Mã tham chiếu ngân hàng
              </span>
              <p className="font-mono text-xs font-semibold text-stone-800 break-all">
                {transaction.semanticKey || transaction.id}
              </p>
            </div>

            <div className="p-4 rounded-2xl bg-stone-50/50 border border-stone-200/60 space-y-1">
              <span className="text-xs font-semibold text-stone-500 flex items-center gap-1.5">
                <Calendar className="w-3.5 h-3.5" />
                Thời điểm ghi nhận
              </span>
              <p className="text-xs font-semibold text-stone-800">
                {transaction.firstSeenAt
                  ? new Date(transaction.firstSeenAt).toLocaleString('vi-VN')
                  : 'Ngay tức thì'}
              </p>
            </div>

            {typeof transaction.balance === 'number' && (
              <div className="p-4 rounded-2xl bg-stone-50/50 border border-stone-200/60 space-y-1 sm:col-span-2">
                <span className="text-xs font-semibold text-stone-500 flex items-center gap-1.5">
                  <Wallet className="w-3.5 h-3.5" />
                  Số dư sau giao dịch
                </span>
                <p className="text-base font-bold text-stone-900">
                  {formatVndCurrency(transaction.balance)}
                </p>
              </div>
            )}
          </div>

          {/* Actions */}
          <div className="pt-4 border-t border-stone-100 flex flex-col sm:flex-row items-center justify-between gap-3">
            <button
              type="button"
              onClick={handleCopy}
              className="w-full sm:w-auto inline-flex items-center justify-center gap-2 px-4 py-2.5 rounded-xl text-xs font-semibold bg-stone-100 hover:bg-stone-200 text-stone-700 transition cursor-pointer"
            >
              {copied ? (
                <>
                  <Check className="w-4 h-4 text-emerald-600" />
                  <span>Đã sao chép vào bộ nhớ tạm!</span>
                </>
              ) : (
                <>
                  <Copy className="w-4 h-4" />
                  <span>Sao chép chi tiết giao dịch</span>
                </>
              )}
            </button>

            {isCredit && (
              <button
                type="button"
                onClick={handleSpeak}
                className="w-full sm:w-auto inline-flex items-center justify-center gap-2 px-4 py-2.5 rounded-xl text-xs font-semibold bg-emerald-50 hover:bg-emerald-100 text-emerald-800 border border-emerald-200 transition cursor-pointer"
              >
                <Volume2 className="w-4 h-4 text-emerald-600" />
                <span>Đọc lại giao dịch</span>
              </button>
            )}
          </div>
        </div>
      </div>
    </div>
  );
};
