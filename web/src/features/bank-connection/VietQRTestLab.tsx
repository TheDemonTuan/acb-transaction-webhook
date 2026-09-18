import React, { useState, useEffect } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import {
  FlaskConical,
  ChevronDown,
  ChevronUp,
  RefreshCw,
  CheckCircle2,
  AlertCircle,
  Copy,
  Check,
  Zap,
  ShieldCheck,
  Radio,
} from 'lucide-react';
import {
  previewPaymentQR,
  fetchCanaryStatus,
  resetCanaryStatus,
  generatePaymentQR,
  VietQRPreviewResponse,
  CanaryStatusResponse,
} from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';

interface VietQRTestLabProps {
  defaultAccountNumber?: string;
  defaultAccountName?: string;
  onPromoted?: () => void;
}

export const VietQRTestLab: React.FC<VietQRTestLabProps> = ({
  defaultAccountNumber = '',
  defaultAccountName = '',
  onPromoted,
}) => {
  const queryClient = useQueryClient();
  const [isOpen, setIsOpen] = useState(false);
  const [accountNumber, setAccountNumber] = useState(defaultAccountNumber);
  const [accountName, setAccountName] = useState(defaultAccountName);
  const [testId, setTestId] = useState('QRTEST01');
  const [copiedField, setCopiedField] = useState<string | null>(null);

  // Sync defaults when parent provides them
  useEffect(() => {
    if (defaultAccountNumber && !accountNumber) {
      setAccountNumber(defaultAccountNumber);
    }
    if (defaultAccountName && !accountName) {
      setAccountName(defaultAccountName);
    }
  }, [defaultAccountNumber, defaultAccountName]);

  const [previews, setPreviews] = useState<{
    standard?: VietQRPreviewResponse;
    reference?: VietQRPreviewResponse;
    hybrid?: VietQRPreviewResponse;
  }>({});
  const [selectedMode, setSelectedMode] = useState<'standard' | 'reference' | 'hybrid'>('standard');
  const [isGenerating, setIsGenerating] = useState(false);
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [successMsg, setSuccessMsg] = useState<string | null>(null);

  // Polling Canary status if hybrid code generated
  const { data: canaryData, refetch: refetchCanary, isFetching: isFetchingCanary } = useQuery<CanaryStatusResponse>({
    queryKey: queryKeys.canaryStatus(testId),
    queryFn: () => fetchCanaryStatus(testId),
    enabled: isOpen && Boolean(previews.hybrid),
    refetchInterval: isOpen && Boolean(previews.hybrid) ? 3000 : false,
  });

  const generateNewTestId = () => {
    const randomHex = Math.random().toString(16).substring(2, 6).toUpperCase();
    setTestId(`QRTEST-${randomHex}`);
  };

  const handleGenerateAll = async () => {
    const accNum = accountNumber.trim() || defaultAccountNumber.trim();
    const accName = accountName.trim() || defaultAccountName.trim();

    if (!accNum) {
      setErrorMsg('Vui lòng nhập Số tài khoản ACB để thử nghiệm!');
      return;
    }

    setErrorMsg(null);
    setSuccessMsg(null);
    setIsGenerating(true);

    try {
      // Call preview API for all 3 modes in parallel
      const [stdRes, refRes, hybRes] = await Promise.all([
        previewPaymentQR({
          accountNumber: accNum,
          accountName: accName,
          mode: 'standard',
          testId,
        }),
        previewPaymentQR({
          accountNumber: accNum,
          accountName: accName,
          mode: 'reference',
          testId,
        }),
        previewPaymentQR({
          accountNumber: accNum,
          accountName: accName,
          mode: 'hybrid',
          testId,
        }),
      ]);

      setPreviews({
        standard: stdRes,
        reference: refRes,
        hybrid: hybRes,
      });
      setSelectedMode('standard');
      setSuccessMsg('Đã tạo thành công 3 mã QR thử nghiệm! (Không ảnh hưởng QR production)');
    } catch (err: any) {
      setErrorMsg(err.message || 'Lỗi khi tạo mã QR thử nghiệm');
    } finally {
      setIsGenerating(false);
    }
  };

  const promoteMutation = useMutation({
    mutationFn: (params: { accountNumber: string; accountName: string }) =>
      generatePaymentQR({ ...params, mode: 'local_standard' }),
    onSuccess: () => {
      setSuccessMsg('Đã áp dụng mã VietQR chuẩn nội bộ làm QR chính thức thành công!');
      queryClient.invalidateQueries({ queryKey: queryKeys.paymentQR });
      if (onPromoted) onPromoted();
    },
    onError: (err: any) => {
      setErrorMsg(`Áp dụng QR chính thất bại: ${err.message}`);
    },
  });

  const resetCanaryMutation = useMutation({
    mutationFn: (token: string) => resetCanaryStatus(token),
    onSuccess: () => {
      refetchCanary();
      setSuccessMsg('Đã đặt lại bộ đếm Canary.');
    },
  });

  const handleCopy = (text: string, field: string) => {
    if (typeof navigator !== 'undefined') {
      navigator.clipboard.writeText(text);
      setCopiedField(field);
      setTimeout(() => setCopiedField(null), 2000);
    }
  };

  const activePreview = previews[selectedMode];

  return (
    <div className="border border-stone-200 rounded-2xl bg-stone-50/50 overflow-hidden">
      {/* Accordion Toggle Header */}
      <button
        type="button"
        onClick={() => setIsOpen(!isOpen)}
        className="w-full px-6 py-4 flex items-center justify-between text-left hover:bg-stone-100/60 transition cursor-pointer"
      >
        <div className="flex items-center gap-3">
          <div className="p-2 rounded-xl bg-amber-100/80 text-amber-800">
            <FlaskConical className="w-5 h-5" />
          </div>
          <div>
            <div className="flex items-center gap-2">
              <h4 className="text-sm font-bold text-stone-900">Phòng thử nghiệm VietQR (VietQR Test Lab)</h4>
              <span className="text-[10px] uppercase font-bold tracking-wider px-2 py-0.5 rounded-full bg-amber-100 text-amber-800 border border-amber-200">
                Sandbox
              </span>
            </div>
            <p className="text-xs text-stone-500 mt-0.5">
              Tạo và kiểm tra đồng thời 3 định dạng mã QR (Standard, +Ref 62, Hybrid 80) hoàn toàn độc lập, không ghi đè production
            </p>
          </div>
        </div>
        <div className="flex items-center gap-2 text-stone-400">
          <span className="text-xs font-medium text-stone-500 hidden sm:inline">
            {isOpen ? 'Thu gọn' : 'Mở công cụ'}
          </span>
          {isOpen ? <ChevronUp className="w-5 h-5" /> : <ChevronDown className="w-5 h-5" />}
        </div>
      </button>

      {/* Lab Body */}
      {isOpen && (
        <div className="p-6 border-t border-stone-200 space-y-6 bg-white">
          {/* Notifications */}
          {errorMsg && (
            <div className="p-3 rounded-xl bg-rose-50 border border-rose-200 text-rose-800 text-xs flex items-center justify-between">
              <div className="flex items-center gap-2">
                <AlertCircle className="w-4 h-4 text-rose-600 shrink-0" />
                <span>{errorMsg}</span>
              </div>
              <button type="button" onClick={() => setErrorMsg(null)} className="text-stone-400 hover:text-stone-600">
                ✕
              </button>
            </div>
          )}

          {successMsg && (
            <div className="p-3 rounded-xl bg-emerald-50 border border-emerald-200 text-emerald-800 text-xs flex items-center justify-between">
              <div className="flex items-center gap-2">
                <CheckCircle2 className="w-4 h-4 text-emerald-600 shrink-0" />
                <span>{successMsg}</span>
              </div>
              <button type="button" onClick={() => setSuccessMsg(null)} className="text-stone-400 hover:text-stone-600">
                ✕
              </button>
            </div>
          )}

          {/* Configuration Form */}
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 bg-stone-50 p-4 rounded-xl border border-stone-200">
            <div>
              <label className="block text-xs font-bold text-stone-700 mb-1">Số tài khoản ACB</label>
              <input
                type="text"
                value={accountNumber}
                onChange={(e) => setAccountNumber(e.target.value)}
                placeholder="VD: 123456789"
                className="w-full px-3 py-2 text-xs font-mono rounded-lg border border-stone-300 bg-white focus:outline-none focus:ring-2 focus:ring-stone-400"
              />
            </div>

            <div>
              <label className="block text-xs font-bold text-stone-700 mb-1">Chủ tài khoản</label>
              <input
                type="text"
                value={accountName}
                onChange={(e) => setAccountName(e.target.value)}
                placeholder="VD: NGUYEN VAN A"
                className="w-full px-3 py-2 text-xs uppercase rounded-lg border border-stone-300 bg-white focus:outline-none focus:ring-2 focus:ring-stone-400"
              />
            </div>

            <div>
              <label className="block text-xs font-bold text-stone-700 mb-1">Mã Test Canary (Tag 62 / 80)</label>
              <div className="flex gap-2">
                <input
                  type="text"
                  value={testId}
                  onChange={(e) => setTestId(e.target.value)}
                  className="w-full px-3 py-2 text-xs font-mono uppercase rounded-lg border border-stone-300 bg-white focus:outline-none focus:ring-2 focus:ring-stone-400"
                />
                <button
                  type="button"
                  onClick={generateNewTestId}
                  className="px-2.5 py-2 text-xs font-medium rounded-lg border border-stone-300 bg-white hover:bg-stone-100 shrink-0 cursor-pointer"
                  title="Tạo ID ngẫu nhiên mới"
                >
                  Đổi ID
                </button>
              </div>
            </div>

            <div className="sm:col-span-3 flex justify-end">
              <button
                type="button"
                onClick={handleGenerateAll}
                disabled={isGenerating}
                className="inline-flex items-center gap-2 px-4 py-2.5 rounded-xl bg-stone-900 text-white text-xs font-semibold hover:bg-stone-800 disabled:opacity-50 transition cursor-pointer shadow-xs"
              >
                {isGenerating ? (
                  <>
                    <RefreshCw className="w-4 h-4 animate-spin" />
                    Đang tạo 3 mã thử nghiệm...
                  </>
                ) : (
                  <>
                    <Zap className="w-4 h-4 text-amber-400" />
                    Tạo 3 mã để kiểm thử ngay
                  </>
                )}
              </button>
            </div>
          </div>

          {/* 3 QR Modes Display */}
          {previews.standard && previews.reference && previews.hybrid && (
            <div className="space-y-6">
              <div className="grid grid-cols-1 md:grid-cols-3 gap-6">
                {/* QR A: STANDARD */}
                <div
                  className={`p-4 rounded-2xl border transition ${
                    selectedMode === 'standard'
                      ? 'border-stone-900 bg-stone-50/80 ring-2 ring-stone-900/10'
                      : 'border-stone-200 bg-white hover:border-stone-300'
                  }`}
                >
                  <div className="flex items-center justify-between mb-3">
                    <span className="text-xs font-bold text-stone-900">A. STANDARD</span>
                    <span className="text-[10px] font-semibold px-2 py-0.5 rounded-full bg-stone-100 text-stone-700">
                      Gốc
                    </span>
                  </div>
                  <div className="bg-white p-3 rounded-xl border border-stone-200 shadow-xs mb-3 text-center">
                    <img
                      src={previews.standard.image}
                      alt="VietQR Standard"
                      className="w-full aspect-square object-contain mx-auto"
                    />
                  </div>
                  <div className="text-[11px] text-stone-600 space-y-1 mb-4">
                    <p className="font-semibold text-stone-900">VietQR chuẩn</p>
                    <p className="text-[10px] text-stone-500 font-mono">
                      AID: A000000727 • BIN: 970416
                    </p>
                    <p className="text-[10px] text-emerald-600 font-medium">
                      ✓ Không có tag tùy biến
                    </p>
                  </div>
                  <div className="space-y-2">
                    <button
                      type="button"
                      onClick={() => setSelectedMode('standard')}
                      className={`w-full py-1.5 px-3 rounded-lg text-xs font-semibold cursor-pointer transition ${
                        selectedMode === 'standard'
                          ? 'bg-stone-900 text-white'
                          : 'bg-stone-100 text-stone-700 hover:bg-stone-200'
                      }`}
                    >
                      Xem chi tiết payload
                    </button>
                    <button
                      type="button"
                      onClick={() =>
                        promoteMutation.mutate({
                          accountNumber: accountNumber.trim(),
                          accountName: accountName.trim(),
                        })
                      }
                      disabled={promoteMutation.isPending}
                      className="w-full py-1.5 px-3 rounded-lg text-xs font-semibold bg-emerald-50 text-emerald-700 border border-emerald-200 hover:bg-emerald-100 transition cursor-pointer flex items-center justify-center gap-1.5"
                    >
                      <ShieldCheck className="w-3.5 h-3.5 text-emerald-600" />
                      Dùng làm QR chính
                    </button>
                  </div>
                </div>

                {/* QR B: REFERENCE 62 */}
                <div
                  className={`p-4 rounded-2xl border transition ${
                    selectedMode === 'reference'
                      ? 'border-stone-900 bg-stone-50/80 ring-2 ring-stone-900/10'
                      : 'border-stone-200 bg-white hover:border-stone-300'
                  }`}
                >
                  <div className="flex items-center justify-between mb-3">
                    <span className="text-xs font-bold text-stone-900">B. + REF 62</span>
                    <span className="text-[10px] font-semibold px-2 py-0.5 rounded-full bg-blue-100 text-blue-800">
                      Tag 62
                    </span>
                  </div>
                  <div className="bg-white p-3 rounded-xl border border-stone-200 shadow-xs mb-3 text-center">
                    <img
                      src={previews.reference.image}
                      alt="VietQR Reference"
                      className="w-full aspect-square object-contain mx-auto"
                    />
                  </div>
                  <div className="text-[11px] text-stone-600 space-y-1 mb-4">
                    <p className="font-semibold text-stone-900">Chuẩn + Reference</p>
                    <p className="text-[10px] text-stone-500 font-mono">
                      62.05: {previews.reference.parsed.reference || testId}
                    </p>
                    <p className="text-[10px] text-blue-600 font-medium">
                      ✓ Mã tham chiếu giao dịch
                    </p>
                  </div>
                  <button
                    type="button"
                    onClick={() => setSelectedMode('reference')}
                    className={`w-full py-1.5 px-3 rounded-lg text-xs font-semibold cursor-pointer transition ${
                      selectedMode === 'reference'
                        ? 'bg-stone-900 text-white'
                        : 'bg-stone-100 text-stone-700 hover:bg-stone-200'
                    }`}
                  >
                    Xem chi tiết payload
                  </button>
                </div>

                {/* QR C: HYBRID 80 */}
                <div
                  className={`p-4 rounded-2xl border transition ${
                    selectedMode === 'hybrid'
                      ? 'border-stone-900 bg-stone-50/80 ring-2 ring-stone-900/10'
                      : 'border-stone-200 bg-white hover:border-stone-300'
                  }`}
                >
                  <div className="flex items-center justify-between mb-3">
                    <span className="text-xs font-bold text-stone-900">C. HYBRID 80</span>
                    <span className="text-[10px] font-semibold px-2 py-0.5 rounded-full bg-purple-100 text-purple-800">
                      Canary
                    </span>
                  </div>
                  <div className="bg-white p-3 rounded-xl border border-stone-200 shadow-xs mb-3 text-center">
                    <img
                      src={previews.hybrid.image}
                      alt="VietQR Hybrid"
                      className="w-full aspect-square object-contain mx-auto"
                    />
                  </div>
                  <div className="text-[11px] text-stone-600 space-y-1 mb-4">
                    <p className="font-semibold text-stone-900">Hybrid Metadata</p>
                    <p className="text-[10px] text-stone-500 font-mono">
                      Tag 80: {previews.hybrid.parsed.customHost || 'Host'}
                    </p>
                    <p className="text-[10px] text-purple-600 font-medium">
                      ✓ Tích hợp token Canary
                    </p>
                  </div>
                  <button
                    type="button"
                    onClick={() => setSelectedMode('hybrid')}
                    className={`w-full py-1.5 px-3 rounded-lg text-xs font-semibold cursor-pointer transition ${
                      selectedMode === 'hybrid'
                        ? 'bg-stone-900 text-white'
                        : 'bg-stone-100 text-stone-700 hover:bg-stone-200'
                    }`}
                  >
                    Xem chi tiết payload
                  </button>
                </div>
              </div>

              {/* Inspector Section for Selected Mode */}
              {activePreview && (
                <div className="p-4 rounded-xl border border-stone-200 bg-stone-50 space-y-4">
                  <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
                    <div className="flex items-center gap-2">
                      <span className="text-xs font-bold text-stone-900 uppercase">
                        Payload định dạng: {selectedMode}
                      </span>
                      <span
                        className={`text-[10px] font-bold px-2 py-0.5 rounded-full ${
                          activePreview.crcValid
                            ? 'bg-emerald-100 text-emerald-800 border border-emerald-200'
                            : 'bg-rose-100 text-rose-800 border border-rose-200'
                        }`}
                      >
                        CRC: {activePreview.crc} {activePreview.crcValid ? '(VALID)' : '(INVALID)'}
                      </span>
                    </div>

                    <button
                      type="button"
                      onClick={() => handleCopy(activePreview.payload, 'payload')}
                      className="inline-flex items-center gap-1.5 text-xs text-stone-600 hover:text-stone-900 cursor-pointer self-start sm:self-auto"
                    >
                      {copiedField === 'payload' ? (
                        <Check className="w-3.5 h-3.5 text-emerald-600" />
                      ) : (
                        <Copy className="w-3.5 h-3.5" />
                      )}
                      <span>Sao chép chuỗi raw</span>
                    </button>
                  </div>

                  <div className="bg-stone-900 text-stone-200 font-mono text-[11px] p-3 rounded-lg break-all select-all">
                    {activePreview.payload}
                  </div>

                  {/* Parsed Fields Summary */}
                  <div className="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs">
                    <div className="bg-white p-2.5 rounded-lg border border-stone-200">
                      <span className="text-[10px] text-stone-400 block">BIN Ngân hàng</span>
                      <span className="font-mono font-bold text-stone-900">
                        {activePreview.parsed.bin || '970416 (ACB)'}
                      </span>
                    </div>
                    <div className="bg-white p-2.5 rounded-lg border border-stone-200">
                      <span className="text-[10px] text-stone-400 block">Số tài khoản</span>
                      <span className="font-mono font-bold text-stone-900">
                        {activePreview.parsed.accountNumber}
                      </span>
                    </div>
                    <div className="bg-white p-2.5 rounded-lg border border-stone-200">
                      <span className="text-[10px] text-stone-400 block">Dịch vụ chuyển khoản</span>
                      <span className="font-mono font-bold text-stone-900">
                        {activePreview.parsed.service || 'QRIBFTTA'}
                      </span>
                    </div>
                    <div className="bg-white p-2.5 rounded-lg border border-stone-200">
                      <span className="text-[10px] text-stone-400 block">Tiền tệ & Quốc gia</span>
                      <span className="font-mono font-bold text-stone-900">
                        {activePreview.parsed.currency || '704'} • {activePreview.parsed.countryCode || 'VN'}
                      </span>
                    </div>
                  </div>
                </div>
              )}

              {/* Canary Telemetry Status Monitor */}
              <div className="border border-purple-200 bg-purple-50/40 rounded-xl p-5 space-y-4">
                <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
                  <div className="flex items-center gap-2">
                    <Radio className="w-4 h-4 text-purple-600 animate-pulse" />
                    <h5 className="text-xs font-bold text-purple-950 uppercase tracking-wide">
                      Canary Scan Telemetry (Giám sát quét mã từ điện thoại)
                    </h5>
                  </div>
                  <div className="flex items-center gap-2">
                    <button
                      type="button"
                      onClick={() => refetchCanary()}
                      disabled={isFetchingCanary}
                      className="inline-flex items-center gap-1 px-2.5 py-1 text-xs font-semibold rounded-lg bg-white border border-purple-200 text-purple-800 hover:bg-purple-100 transition cursor-pointer"
                    >
                      <RefreshCw className={`w-3 h-3 ${isFetchingCanary ? 'animate-spin' : ''}`} />
                      Làm mới
                    </button>
                    <button
                      type="button"
                      onClick={() => resetCanaryMutation.mutate(testId)}
                      disabled={resetCanaryMutation.isPending}
                      className="px-2.5 py-1 text-xs font-medium rounded-lg text-stone-600 hover:text-stone-900 border border-stone-200 bg-white transition cursor-pointer"
                    >
                      Đặt lại
                    </button>
                  </div>
                </div>

                <p className="text-xs text-stone-600">
                  Cầm điện thoại mở <strong>ACB ONE</strong>, <strong>MB Bank</strong>, hoặc <strong>Vietcombank</strong> quét mã <strong>C. HYBRID 80</strong> (chưa cần chuyển tiền). Nếu app ngân hàng gửi yêu cầu tải trước, hệ thống sẽ ghi nhận ngay lập tức tại đây:
                </p>

                <div className="grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs">
                  <div className="bg-white p-3 rounded-lg border border-purple-100 shadow-2xs">
                    <span className="text-[10px] text-stone-500 block">Canary Token</span>
                    <span className="font-mono font-bold text-purple-900 break-all">{testId}</span>
                  </div>

                  <div className="bg-white p-3 rounded-lg border border-purple-100 shadow-2xs">
                    <span className="text-[10px] text-stone-500 block">Số lượt quét (Hits)</span>
                    <span className={`text-base font-extrabold ${canaryData && canaryData.hitCount > 0 ? 'text-emerald-600' : 'text-stone-700'}`}>
                      {canaryData?.hitCount ?? 0}
                    </span>
                  </div>

                  <div className="bg-white p-3 rounded-lg border border-purple-100 shadow-2xs">
                    <span className="text-[10px] text-stone-500 block">Lần quét cuối</span>
                    <span className="text-stone-700 font-medium">
                      {canaryData?.lastHitAt ? new Date(canaryData.lastHitAt).toLocaleTimeString() : '-'}
                    </span>
                  </div>

                  <div className="bg-white p-3 rounded-lg border border-purple-100 shadow-2xs">
                    <span className="text-[10px] text-stone-500 block">IP nguồn</span>
                    <span className="font-mono text-stone-700">{canaryData?.lastSourceIp || '-'}</span>
                  </div>
                </div>

                {canaryData?.lastUserAgent && (
                  <div className="bg-white p-2.5 rounded-lg border border-purple-100 text-[11px] text-stone-600">
                    <span className="font-bold text-stone-700">User-Agent detected: </span>
                    <span className="font-mono text-[10px] text-stone-500 break-all">{canaryData.lastUserAgent}</span>
                  </div>
                )}
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
};
