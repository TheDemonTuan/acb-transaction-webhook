import React, { useEffect, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Landmark,
  Save,
  LogIn,
  RotateCcw,
  RefreshCw,
} from 'lucide-react';
import { configureConnection, fetchConnection, fetchStatus } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { getAcbStatusDescriptor } from '../../content/status-copy';
import { useBankConnection } from '../../features/bank-connection/BankConnectionProvider';
import { ScheduleSettingsSection } from '../../features/bank-connection/ScheduleSettingsSection';
import { PaymentQRSettingsSection } from '../../features/bank-connection/PaymentQRSettingsSection';

export const BankConnectionPage: React.FC = () => {
  const queryClient = useQueryClient();
  const [accountInput, setAccountInput] = useState('');
  const [isSaving, setIsSaving] = useState(false);

  const {
    activeAttempt,
    hasActiveAuth,
    browserUnavailable,
    startAuth,
    cancelAuth,
    sync,
    reloadScreen,
    screenKey,
    isStartingAuth,
    isCancelling,
    isSyncing,
    setGlobalNotice,
  } = useBankConnection();

  const { data: connData, isLoading, refetch } = useQuery({
    queryKey: queryKeys.connection,
    queryFn: fetchConnection,
  });

  const { data: statusData } = useQuery({
    queryKey: queryKeys.status,
    queryFn: fetchStatus,
  });

  const connection = connData?.connection;
  const acbState = connection?.state || statusData?.acb?.state || 'UNCONFIGURED';
  const desc = getAcbStatusDescriptor(acbState);
  const isMonitoring = acbState === 'MONITORING';
  const connected =
    connData?.configured ||
    Boolean(connection?.accountMasked) ||
    Boolean(statusData?.acb?.accountMasked) ||
    (acbState !== 'UNCONFIGURED' && acbState !== '');

  useEffect(() => {
    if (connection?.accountMasked) {
      setAccountInput(connection.accountMasked);
    }
  }, [connection?.accountMasked]);

  const handleSaveConnection = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!accountInput.trim()) return;
    setIsSaving(true);
    setGlobalNotice(null);

    try {
      const res = await configureConnection(accountInput.trim());
      const connData = (res as any)?.connection ? res : { configured: true, connection: res };
      queryClient.setQueryData(queryKeys.connection, connData);
      setGlobalNotice({ kind: 'ok', text: 'Đã lưu kết nối.' });
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.connection }),
        queryClient.invalidateQueries({ queryKey: queryKeys.status }),
      ]);
    } catch (err: any) {
      setGlobalNotice({
        kind: 'error',
        text: err instanceof Error ? err.message : 'Không thể lưu kết nối.',
      });
    } finally {
      setIsSaving(false);
    }
  };

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h2 className="text-2xl font-bold tracking-tight text-stone-900">Kết nối ACB</h2>
          <p className="text-sm text-stone-500 mt-0.5">
            Cấu hình tài khoản ngân hàng và phiên đăng nhập bảo mật
          </p>
        </div>
        <button
          type="button"
          onClick={() => refetch()}
          disabled={isLoading}
          className="inline-flex items-center self-start sm:self-auto gap-2 px-3 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer"
        >
          <RefreshCw className={`w-3.5 h-3.5 ${isLoading ? 'animate-spin' : ''}`} />
          <span>Làm mới</span>
        </button>
      </div>

      {/* Connection status card */}
      <div className="bg-white p-6 rounded-2xl border border-stone-200 shadow-2xs">
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
          <div className="flex items-center gap-3.5">
            <div className="w-12 h-12 rounded-2xl bg-stone-100 flex items-center justify-center text-stone-700">
              <Landmark className="w-6 h-6" />
            </div>
            <div>
              <div className="flex items-center gap-2">
                <h3 className="font-bold text-stone-900 text-base">Trạng thái kết nối</h3>
                <span className="text-xs px-2.5 py-0.5 rounded-full bg-stone-100 border border-stone-200 font-semibold text-stone-700 flex items-center gap-1.5">
                  <span>{desc.badge}</span>
                  <span className="font-mono text-[11px] font-normal text-stone-400">({acbState})</span>
                </span>
              </div>
              <p className="text-xs text-stone-500 mt-0.5">
                {isMonitoring ? 'Phiên ACB đang hoạt động bình thường' : desc.label}
              </p>
            </div>
          </div>

          <div className="flex items-center gap-2">
            <button
              type="button"
              role="button"
              aria-label="Sync"
              onClick={() => sync().catch(() => {})}
              disabled={!isMonitoring || hasActiveAuth || isSyncing}
              className="inline-flex items-center gap-1.5 px-3.5 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed"
            >
              <RotateCcw className={`w-3.5 h-3.5 ${isSyncing ? 'animate-spin' : ''}`} />
              <span>Đồng bộ ngay (Sync)</span>
            </button>
          </div>
        </div>
      </div>

      {/* Account Configuration Form (only when not connected) */}
      {!connected && (
        <div className="bg-white p-6 rounded-2xl border border-stone-200 shadow-2xs">
          <h3 className="font-bold text-stone-900 text-base mb-1">Cấu hình tài khoản</h3>
          <p className="text-xs text-stone-500 mb-4">
            Nhập số tài khoản ACB cần nhận webhook biến động số dư.
          </p>

          <form onSubmit={handleSaveConnection} className="space-y-4 max-w-md">
            <div>
              <label
                htmlFor="accountMasked"
                className="block text-xs font-medium text-stone-700 mb-1"
              >
                Số tài khoản đã che
              </label>
              <input
                id="accountMasked"
                type="text"
                aria-label="Số tài khoản đã che"
                value={accountInput}
                onChange={(e) => setAccountInput(e.target.value)}
                placeholder="ví dụ: ***1234"
                className="w-full px-3.5 py-2 text-sm rounded-xl border border-stone-200 focus:outline-none focus:ring-2 focus:ring-emerald-500/20 focus:border-emerald-500 font-mono"
              />
            </div>

            <button
              type="submit"
              disabled={isSaving}
              className="inline-flex items-center gap-2 px-4 py-2.5 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 transition shadow-xs cursor-pointer disabled:opacity-50"
            >
              <Save className="w-3.5 h-3.5" />
              <span>{isSaving ? 'Đang lưu...' : 'Lưu kết nối'}</span>
            </button>
          </form>
        </div>
      )}

      {/* Login & Verification section */}
      <div className="bg-white p-6 rounded-2xl border border-stone-200 shadow-2xs space-y-4">
        <div>
          <h3 className="font-bold text-stone-900 text-base">
            Đăng nhập &amp; Xác thực ACB
          </h3>
          <p className="text-xs text-stone-500 mt-0.5">
            Mở phiên trình duyệt tự động để đăng nhập ACB an toàn.
          </p>
        </div>

        {!activeAttempt ? (
          <div>
            <button
              type="button"
              onClick={startAuth}
              disabled={isStartingAuth}
              className="inline-flex items-center gap-2 px-5 py-2.5 rounded-xl text-xs font-semibold bg-emerald-600 text-white hover:bg-emerald-700 transition shadow-xs cursor-pointer disabled:opacity-50"
            >
              <LogIn className="w-4 h-4" />
              <span>
                {isStartingAuth ? 'Đang mở trình duyệt...' : 'Bắt đầu đăng nhập ACB'}
              </span>
            </button>
          </div>
        ) : (
          <div className="space-y-4 border border-stone-200 rounded-2xl p-4 bg-stone-50/50">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-2 text-xs font-medium">
                {browserUnavailable ? (
                  <>
                    <span className="w-2 h-2 rounded-full bg-amber-500 animate-pulse" />
                    <span className="text-amber-700">
                      Dịch vụ trình duyệt ACB tạm thời gián đoạn. Đang kết nối lại...
                    </span>
                  </>
                ) : (
                  <>
                    <span className="w-2 h-2 rounded-full bg-emerald-500 animate-pulse" />
                    <span className="text-emerald-700">
                      Trình duyệt ACB đã sẵn sàng. Bạn có thể thao tác qua VNC bên dưới.
                    </span>
                  </>
                )}
              </div>
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  onClick={reloadScreen}
                  className="px-3 py-1.5 rounded-lg text-xs font-medium bg-stone-100 text-stone-700 hover:bg-stone-200 transition cursor-pointer inline-flex items-center gap-1.5"
                  title="Tải lại kết nối màn hình VNC nếu bị ngắt kết nối"
                >
                  <RotateCcw className="w-3.5 h-3.5" />
                  <span>Tải lại VNC</span>
                </button>
                <button
                  type="button"
                  onClick={cancelAuth}
                  disabled={isCancelling}
                  className="px-3 py-1.5 rounded-lg text-xs font-medium bg-rose-50 text-rose-700 hover:bg-rose-100 transition cursor-pointer"
                >
                  Hủy phiên đăng nhập
                </button>
              </div>
            </div>

            {activeAttempt.screenUrl && (
              <div className="rounded-xl overflow-hidden border border-stone-200 bg-white aspect-video max-h-[500px] w-full">
                <iframe
                  key={screenKey}
                  title="Đăng nhập ACB"
                  src={activeAttempt.screenUrl}
                  className="w-full h-full border-0"
                />
              </div>
            )}
          </div>
        )}
      </div>

      {/* Schedule Polling Settings */}
      <ScheduleSettingsSection />

      {/* Static Payment QR Settings */}
      <PaymentQRSettingsSection />
    </div>
  );
};
