import React, { useRef, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import {
  QrCode,
  Upload,
  Sparkles,
  Trash2,
  CheckCircle2,
  AlertTriangle,
  RefreshCw,
  Copy,
  Check,
  ExternalLink,
} from 'lucide-react';
import {
  fetchPaymentQR,
  uploadPaymentQR,
  generatePaymentQR,
  deletePaymentQR,
} from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';

export const PaymentQRSettingsSection: React.FC = () => {
  const queryClient = useQueryClient();
  const fileInputRef = useRef<HTMLInputElement>(null);

  const [accountNumber, setAccountNumber] = useState('');
  const [accountName, setAccountName] = useState('');
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; message: string } | null>(null);
  const [copied, setCopied] = useState(false);

  const { data, isLoading, refetch } = useQuery({
    queryKey: queryKeys.paymentQR,
    queryFn: fetchPaymentQR,
  });

  const qr = data?.qr;
  const isConfigured = data?.configured && data?.hasImage;

  const uploadMutation = useMutation({
    mutationFn: (formData: FormData) => uploadPaymentQR(formData),
    onSuccess: () => {
      setNotice({ kind: 'success', message: 'Tải lên mã QR nhận tiền thành công!' });
      queryClient.invalidateQueries({ queryKey: queryKeys.paymentQR });
    },
    onError: (err: any) => {
      setNotice({ kind: 'error', message: `Tải lên thất bại: ${err.message}` });
    },
  });

  const generateMutation = useMutation({
    mutationFn: (params: { accountNumber: string; accountName: string }) =>
      generatePaymentQR(params),
    onSuccess: () => {
      setNotice({ kind: 'success', message: 'Tạo mã VietQR tự động thành công!' });
      queryClient.invalidateQueries({ queryKey: queryKeys.paymentQR });
    },
    onError: (err: any) => {
      setNotice({ kind: 'error', message: `Tạo mã thất bại: ${err.message}` });
    },
  });

  const deleteMutation = useMutation({
    mutationFn: () => deletePaymentQR(),
    onSuccess: () => {
      setNotice({ kind: 'success', message: 'Đã xóa mã QR nhận tiền.' });
      queryClient.invalidateQueries({ queryKey: queryKeys.paymentQR });
    },
    onError: (err: any) => {
      setNotice({ kind: 'error', message: `Xóa thất bại: ${err.message}` });
    },
  });

  const handleFileUpload = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;

    const accNum = accountNumber.trim() || qr?.accountNumber;
    const accName = accountName.trim() || qr?.accountName;

    if (!accNum || !accName) {
      setNotice({
        kind: 'error',
        message: 'Vui lòng nhập Số tài khoản và Tên chủ tài khoản trước khi tải ảnh QR lên!',
      });
      return;
    }

    const formData = new FormData();
    formData.append('image', file);
    formData.append('accountNumber', accNum);
    formData.append('accountName', accName);
    uploadMutation.mutate(formData);
  };

  const handleGenerate = () => {
    const accNum = accountNumber.trim() || qr?.accountNumber;
    const accName = accountName.trim() || qr?.accountName;

    if (!accNum || !accName) {
      setNotice({
        kind: 'error',
        message: 'Vui lòng nhập Số tài khoản và Tên chủ tài khoản để tạo mã QR tự động!',
      });
      return;
    }

    generateMutation.mutate({ accountNumber: accNum, accountName: accName });
  };

  const handleCopy = (text: string) => {
    if (typeof navigator !== 'undefined') {
      navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  if (isLoading) {
    return (
      <div className="bg-white p-6 rounded-2xl border border-stone-200/80 shadow-xs text-center text-xs text-stone-500">
        <RefreshCw className="w-5 h-5 animate-spin mx-auto mb-2 text-stone-400" />
        Đang tải thông tin mã QR...
      </div>
    );
  }

  return (
    <div className="bg-white rounded-2xl border border-stone-200/80 shadow-xs overflow-hidden">
      {/* Header */}
      <div className="p-6 border-b border-stone-100 flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <div className="flex items-center gap-2">
            <QrCode className="w-5 h-5 text-stone-700" />
            <h3 className="text-base font-bold text-stone-900">Mã QR tĩnh nhận tiền (Payment QR)</h3>
          </div>
          <p className="text-xs text-stone-500 mt-1">
            Hiển thị mã QR nhận tiền tĩnh trên Transaction Viewer để đối tác và khách hàng quét nhanh
          </p>
        </div>

        <button
          type="button"
          onClick={() => refetch()}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-semibold bg-stone-50 border border-stone-200 text-stone-600 hover:bg-stone-100 transition cursor-pointer self-start sm:self-auto"
        >
          <RefreshCw className="w-3.5 h-3.5" />
          Làm mới
        </button>
      </div>

      <div className="p-6 space-y-6">
        {notice && (
          <div
            className={`p-4 rounded-xl text-xs font-medium flex items-center justify-between gap-2 ${
              notice.kind === 'success'
                ? 'bg-emerald-50 text-emerald-800 border border-emerald-200'
                : 'bg-rose-50 text-rose-800 border border-rose-200'
            }`}
          >
            <div className="flex items-center gap-2">
              {notice.kind === 'success' ? (
                <CheckCircle2 className="w-4 h-4 shrink-0 text-emerald-600" />
              ) : (
                <AlertTriangle className="w-4 h-4 shrink-0 text-rose-600" />
              )}
              <span>{notice.message}</span>
            </div>
            <button
              type="button"
              onClick={() => setNotice(null)}
              className="text-stone-400 hover:text-stone-600 font-bold"
            >
              &times;
            </button>
          </div>
        )}

        <div className="grid grid-cols-1 md:grid-cols-2 gap-6 items-start">
          {/* Form setup */}
          <div className="space-y-4 text-xs">
            <div>
              <label className="font-bold text-stone-800 block mb-1">Số tài khoản ACB</label>
              <input
                type="text"
                value={accountNumber}
                placeholder={qr?.accountNumber || 'Ví dụ: 123456789'}
                onChange={(e) => setAccountNumber(e.target.value)}
                className="w-full px-3 py-2 bg-stone-50 border border-stone-200 rounded-xl text-xs font-mono text-stone-900 focus:outline-none focus:ring-2 focus:ring-stone-900/10 focus:border-stone-900"
              />
            </div>

            <div>
              <label className="font-bold text-stone-800 block mb-1">Tên chủ tài khoản (In hoa không dấu)</label>
              <input
                type="text"
                value={accountName}
                placeholder={qr?.accountName || 'Ví dụ: NGUYEN VAN A'}
                onChange={(e) => setAccountName(e.target.value.toUpperCase())}
                className="w-full px-3 py-2 bg-stone-50 border border-stone-200 rounded-xl text-xs text-stone-900 uppercase focus:outline-none focus:ring-2 focus:ring-stone-900/10 focus:border-stone-900"
              />
            </div>

            <div className="pt-2 flex flex-wrap gap-2.5">
              <input
                type="file"
                ref={fileInputRef}
                onChange={handleFileUpload}
                accept="image/png,image/jpeg,image/webp"
                className="hidden"
              />
              <button
                type="button"
                onClick={() => fileInputRef.current?.click()}
                disabled={uploadMutation.isPending}
                className="inline-flex items-center gap-1.5 px-4 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-300 text-stone-700 hover:bg-stone-50 shadow-2xs transition cursor-pointer disabled:opacity-50"
              >
                <Upload className="w-3.5 h-3.5" />
                {uploadMutation.isPending ? 'Đang tải lên...' : 'Tải ảnh QR có sẵn'}
              </button>

              <button
                type="button"
                onClick={handleGenerate}
                disabled={generateMutation.isPending}
                className="inline-flex items-center gap-1.5 px-4 py-2 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 shadow-2xs transition cursor-pointer disabled:opacity-50"
              >
                <Sparkles className="w-3.5 h-3.5 text-amber-400" />
                {generateMutation.isPending ? 'Đang tạo QR...' : 'Tạo VietQR tự động'}
              </button>

              {isConfigured && (
                <button
                  type="button"
                  onClick={() => deleteMutation.mutate()}
                  disabled={deleteMutation.isPending}
                  className="inline-flex items-center gap-1.5 px-3 py-2 rounded-xl text-xs font-semibold bg-rose-50 text-rose-700 hover:bg-rose-100 transition cursor-pointer disabled:opacity-50"
                  title="Xóa mã QR hiện tại"
                >
                  <Trash2 className="w-3.5 h-3.5" />
                  Xóa QR
                </button>
              )}
            </div>
          </div>

          {/* QR Preview Card */}
          <div className="bg-stone-50/60 p-5 rounded-2xl border border-stone-200/80 flex flex-col items-center justify-center text-center">
            {isConfigured ? (
              <div className="space-y-3 w-full max-w-[240px]">
                <div className="bg-white p-3 rounded-2xl shadow-xs border border-stone-200">
                  <img
                    src={data.imageURL}
                    alt="Mã QR nhận tiền ACB"
                    className="w-full aspect-square object-contain rounded-xl"
                  />
                </div>
                <div className="text-xs space-y-1">
                  <p className="font-bold text-stone-900">{qr?.accountName}</p>
                  <div className="flex items-center justify-center gap-1 font-mono text-stone-600">
                    <span>{qr?.accountNumber}</span>
                    <button
                      type="button"
                      onClick={() => handleCopy(qr?.accountNumber || '')}
                      className="p-1 hover:text-stone-900 cursor-pointer"
                    >
                      {copied ? (
                        <Check className="w-3.5 h-3.5 text-emerald-600" />
                      ) : (
                        <Copy className="w-3.5 h-3.5" />
                      )}
                    </button>
                  </div>
                  <p className="text-[10px] text-stone-500">
                    Ngân hàng TMCP Á Châu (ACB) • Nguồn: {qr?.provider}
                  </p>
                </div>
              </div>
            ) : (
              <div className="py-8 space-y-2 text-stone-400">
                <QrCode className="w-12 h-12 mx-auto stroke-1" />
                <p className="text-xs font-medium text-stone-600">Chưa cấu hình mã QR nhận tiền</p>
                <p className="text-[11px] text-stone-400 max-w-[200px] mx-auto">
                  Nhập số tài khoản và bấm "Tạo VietQR tự động" hoặc tải ảnh mã QR của bạn lên.
                </p>
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
};
