import React from 'react';
import { useQuery } from '@tanstack/react-query';
import { useNavigate } from 'react-router-dom';
import {
  CheckCircle2,
  AlertTriangle,
  Receipt,
  ArrowRight,
  Bell,
  RefreshCw,
  Landmark,
  ShieldCheck,
} from 'lucide-react';
import { fetchConnection, fetchNotificationChannels, fetchStatus } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { getAcbStatusDescriptor } from '../../content/status-copy';

export const OverviewPage: React.FC = () => {
  const navigate = useNavigate();

  const { data: status, isLoading: loadingStatus, refetch: refetchStatus } = useQuery({
    queryKey: queryKeys.status,
    queryFn: fetchStatus,
  });

  const { data: connData, isLoading: loadingConn, refetch: refetchConn } = useQuery({
    queryKey: queryKeys.connection,
    queryFn: fetchConnection,
  });

  const { data: channelsData } = useQuery({
    queryKey: queryKeys.notificationChannels,
    queryFn: fetchNotificationChannels,
  });

  const acbState = connData?.connection?.state || status?.acb?.state || 'UNCONFIGURED';
  const desc = getAcbStatusDescriptor(acbState);
  const isMonitoring = acbState === 'MONITORING';
  const activeChannelsCount =
    channelsData?.items?.filter((w) => w.status === 'ACTIVE').length ?? 0;

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h2 className="text-2xl font-bold tracking-tight text-stone-900">Tổng quan</h2>
          <p className="text-sm text-stone-500 mt-0.5">
            Trạng thái hoạt động và các chỉ số vận hành cổng giao dịch ACB
          </p>
        </div>
        <button
          type="button"
          onClick={() => {
            refetchStatus();
            refetchConn();
          }}
          className="inline-flex items-center self-start sm:self-auto gap-2 px-3 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer"
        >
          <RefreshCw className={`w-3.5 h-3.5 ${loadingStatus || loadingConn ? 'animate-spin' : ''}`} />
          <span>Làm mới dữ liệu</span>
        </button>
      </div>

      {/* Primary Status Banner */}
      <div
        className={`p-6 rounded-2xl border ${
          isMonitoring
            ? 'bg-emerald-50/70 border-emerald-200 text-emerald-900'
            : acbState === 'AUTH_REQUIRED'
            ? 'bg-amber-50/70 border-amber-200 text-amber-900'
            : 'bg-stone-100 border-stone-200 text-stone-800'
        } flex flex-col sm:flex-row sm:items-center justify-between gap-4`}
      >
        <div className="flex items-start gap-4">
          <div
            className={`p-3 rounded-2xl shrink-0 ${
              isMonitoring
                ? 'bg-emerald-100 text-emerald-700'
                : acbState === 'AUTH_REQUIRED'
                ? 'bg-amber-100 text-amber-700'
                : 'bg-stone-200 text-stone-600'
            }`}
          >
            {isMonitoring ? (
              <CheckCircle2 className="w-6 h-6" />
            ) : (
              <AlertTriangle className="w-6 h-6" />
            )}
          </div>
          <div>
            <div className="flex items-center gap-2.5">
              <h3 className="text-lg font-bold">
                {isMonitoring
                  ? 'Phiên ACB đang hoạt động bình thường'
                  : acbState === 'AUTH_REQUIRED'
                  ? 'ACB chờ đăng nhập qua Telegram'
                  : desc.label}
              </h3>
              <span className="text-xs font-semibold px-2.5 py-0.5 rounded-full bg-white/80 border border-stone-200 text-stone-800 flex items-center gap-1.5">
                <span>{desc.badge}</span>
                <span className="font-mono text-[11px] font-normal text-stone-400">({acbState})</span>
              </span>
            </div>
            <p className="text-xs text-stone-600 mt-1 max-w-xl">
              {desc.description ||
                'Hệ thống đang tự động lắng nghe và đẩy webhook tức thì khi có biến động số dư.'}
            </p>
          </div>
        </div>

        <div className="flex items-center gap-2 self-start sm:self-auto shrink-0">
          {acbState !== 'MONITORING' ? (
            <button
              type="button"
              onClick={() => navigate('/admin/connection')}
              className="px-4 py-2 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 transition shadow-xs cursor-pointer"
            >
              Xem trạng thái kết nối
            </button>
          ) : (
            <button
              type="button"
              onClick={() => navigate('/transactions')}
              className="inline-flex items-center gap-2 px-4 py-2 rounded-xl text-xs font-semibold bg-emerald-600 text-white hover:bg-emerald-700 transition shadow-xs cursor-pointer"
            >
              <Receipt className="w-4 h-4" />
              <span>Theo dõi biến động</span>
            </button>
          )}
        </div>
      </div>

      {/* KPI Cards */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-stone-500 uppercase tracking-wider">
              Tài khoản kết nối
            </span>
            <span className="text-xs font-semibold px-2 py-0.5 rounded-full bg-emerald-50 text-emerald-700 border border-emerald-200 font-mono">
              {connData?.connection?.accountMasked || status?.acb?.accountMasked || 'Chưa cấu hình'}
            </span>
          </div>
          <div className="mt-4">
            <span className="text-sm font-semibold text-stone-800 block">
              Trạng thái: <span className="text-emerald-700">{desc.label}</span>
            </span>
            <span className="text-xs text-stone-400 mt-0.5 block">
              Kết nối trực tiếp qua kênh bảo mật
            </span>
          </div>
        </div>

        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-stone-500 uppercase tracking-wider">
              Kênh thông báo
            </span>
            <div className="p-2 rounded-xl bg-blue-50 text-blue-600">
              <Bell className="w-4 h-4" />
            </div>
          </div>
          <div className="mt-3">
            <span className="text-2xl font-bold tracking-tight text-stone-900">
              {activeChannelsCount}
            </span>
            <span className="text-xs text-stone-500 ml-1.5 font-medium">kênh đang bật</span>
          </div>
        </div>

        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-stone-500 uppercase tracking-wider">
              Hạ tầng Gateway
            </span>
            <div className="p-2 rounded-xl bg-emerald-50 text-emerald-600">
              <ShieldCheck className="w-4 h-4" />
            </div>
          </div>
          <div className="mt-3">
            <span className="text-sm font-semibold text-stone-800 block">
              Hoạt động ổn định
            </span>
            <span className="text-xs text-stone-500 block mt-0.5">
              Thời gian chạy: {Math.floor((status?.uptimeSeconds || 0) / 60)} phút
            </span>
          </div>
        </div>
      </div>

      {/* Quick Links Section */}
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
        <div
          onClick={() => navigate('/transactions')}
          className="bg-white p-6 rounded-2xl border border-stone-200 shadow-2xs hover:border-emerald-300 transition cursor-pointer group"
        >
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-3">
              <div className="p-2.5 rounded-xl bg-emerald-50 text-emerald-600 group-hover:bg-emerald-600 group-hover:text-white transition">
                <Receipt className="w-5 h-5" />
              </div>
              <div>
                <h4 className="font-bold text-stone-900">Màn hình xem giao dịch (Transaction Viewer)</h4>
                <p className="text-xs text-stone-500">
                  Giao diện độc lập theo dõi giao dịch và phát âm thanh tiếng Việt
                </p>
              </div>
            </div>
            <ArrowRight className="w-4 h-4 text-stone-400 group-hover:text-emerald-600 group-hover:translate-x-1 transition" />
          </div>
        </div>

        <div
          onClick={() => navigate('/admin/notifications')}
          className="bg-white p-6 rounded-2xl border border-stone-200 shadow-2xs hover:border-blue-300 transition cursor-pointer group"
        >
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-3">
              <div className="p-2.5 rounded-xl bg-blue-50 text-blue-600 group-hover:bg-blue-600 group-hover:text-white transition">
                <Bell className="w-5 h-5" />
              </div>
              <div>
                <h4 className="font-bold text-stone-900">Cấu hình kênh thông báo</h4>
                <p className="text-xs text-stone-500">
                  Quản lý các URL đích nhận thông báo giao dịch tự động
                </p>
              </div>
            </div>
            <ArrowRight className="w-4 h-4 text-stone-400 group-hover:text-blue-600 group-hover:translate-x-1 transition" />
          </div>
        </div>
      </div>
    </div>
  );
};
