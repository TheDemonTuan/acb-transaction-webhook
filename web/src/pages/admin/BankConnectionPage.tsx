import React, { useEffect, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Landmark,
  Save,
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

  const { sync, isSyncing, setGlobalNotice } = useBankConnection();

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
      // Keep the submitted connection visible while background status catches up.
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.connection, refetchType: 'none' }),
        queryClient.invalidateQueries({ queryKey: queryKeys.status, refetchType: 'none' }),
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
            Trạng thái phiên ACB, lịch theo dõi và mã QR nhận tiền
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
              disabled={!isMonitoring || isSyncing}
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

      <section className="bg-white p-6 rounded-2xl border border-stone-200 shadow-2xs space-y-3">
        <h3 className="font-bold text-stone-900 text-base">
          Phiên ACB được quản lý qua Telegram
        </h3>
        <p className="text-sm text-stone-600 leading-relaxed">
          Mở menu bot trong chat riêng để đăng nhập, hủy lượt đăng nhập, đổi thông tin đã lưu
          hoặc đăng xuất ACB. Hệ thống chỉ bắt đầu đăng nhập khi bạn bấm nút Đăng nhập trong Telegram;
          mã OTP đăng nhập được trả lời trực tiếp vào tin nhắn yêu cầu của bot.
        </p>
        {!connected && (
          <p className="text-sm text-amber-800">
            Chưa khởi tạo — chạy setup/import trên VPS để lưu thông tin đăng nhập và số tài khoản theo dõi.
          </p>
        )}
        {connData?.authRecovery && (
          <dl className="text-xs text-stone-600 space-y-2">
            <div className="flex flex-wrap gap-2">
              <dt>Trạng thái xử lý:</dt>
              <dd className="font-mono break-all">{connData.authRecovery.state}</dd>
            </div>
            {connData.authRecovery.reasonCode && (
              <div className="flex flex-wrap gap-2">
                <dt>Mã trạng thái:</dt>
                <dd className="font-mono break-all">{connData.authRecovery.reasonCode}</dd>
              </div>
            )}
            <div className="flex flex-wrap gap-2">
              <dt>Cập nhật lúc:</dt>
              <dd>{connData.authRecovery.updatedAt}</dd>
            </div>
          </dl>
        )}
        <p className="text-xs text-stone-500">
          Trang này chỉ hiển thị trạng thái và thiết lập theo dõi; không có màn hình đăng nhập ngân hàng.
        </p>
      </section>

      {/* Schedule Polling Settings */}
      <ScheduleSettingsSection />

      {/* Static Payment QR Settings */}
      <PaymentQRSettingsSection />
    </div>
  );
};
