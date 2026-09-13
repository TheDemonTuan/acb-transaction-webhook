import React from 'react';
import { useQuery } from '@tanstack/react-query';
import {
  Server,
  Database,
  Cpu,
  RefreshCw,
  CheckCircle2,
  AlertCircle,
  Activity,
} from 'lucide-react';
import { fetchStatus } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';

export const SystemPage: React.FC = () => {
  const { data: status, isLoading, refetch } = useQuery({
    queryKey: queryKeys.status,
    queryFn: fetchStatus,
  });

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h2 className="text-2xl font-bold tracking-tight text-stone-900">
            Chẩn đoán hệ thống
          </h2>
          <p className="text-sm text-stone-500 mt-0.5">
            Thông số kỹ thuật, tình trạng tài nguyên lưu trữ và nhật ký tiến trình
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

      {/* Health Overview */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-stone-500 uppercase tracking-wider">
              Dịch vụ lõi
            </span>
            <div className="p-2 rounded-xl bg-emerald-50 text-emerald-600">
              <Server className="w-4 h-4" />
            </div>
          </div>
          <div className="mt-3">
            <div className="flex items-center gap-2">
              <span className="text-xl font-bold text-stone-900">
                {status?.service || 'HEALTHY'}
              </span>
              <CheckCircle2 className="w-4 h-4 text-emerald-600" />
            </div>
            <span className="text-xs text-stone-500 block mt-1">
              Phiên bản hệ thống: v{status?.version || '2.0.0'}
            </span>
          </div>
        </div>

        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-stone-500 uppercase tracking-wider">
              Thời gian chạy (Uptime)
            </span>
            <div className="p-2 rounded-xl bg-blue-50 text-blue-600">
              <Activity className="w-4 h-4" />
            </div>
          </div>
          <div className="mt-3">
            <span className="text-xl font-bold text-stone-900">
              {Math.floor((status?.uptimeSeconds || 0) / 60)} phút
            </span>
            <span className="text-xs text-stone-500 block mt-1">
              ({status?.uptimeSeconds || 0} giây)
            </span>
          </div>
        </div>

        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-stone-500 uppercase tracking-wider">
              Cơ sở dữ liệu
            </span>
            <div className="p-2 rounded-xl bg-emerald-50 text-emerald-600">
              <Database className="w-4 h-4" />
            </div>
          </div>
          <div className="mt-3">
            <span className="text-xl font-bold text-stone-900">
              Sẵn sàng hoạt động
            </span>
            <span className="text-xs text-stone-500 block mt-1">
              Chế độ an toàn cao &middot; Giao dịch chuẩn ACID
            </span>
          </div>
        </div>
      </div>

      {/* Technical Diagnostics Details */}
      <div className="bg-white p-6 rounded-2xl border border-stone-200 shadow-2xs space-y-4">
        <h3 className="font-bold text-stone-900 text-base">Thông số vận hành chi tiết</h3>

        <div className="divide-y divide-stone-100 text-xs font-mono">
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Thế hệ phiên kết nối:</span>
            <span className="font-bold text-stone-900">{status?.acb?.generation ?? 1}</span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Mức độ bao phủ giao dịch:</span>
            <span className="font-bold text-stone-900">
              {status?.acb?.coverage === 'FULL'
                ? 'Đầy đủ (FULL)'
                : status?.acb?.coverage === 'PARTIAL'
                ? 'Một phần (PARTIAL)'
                : 'Chưa bắt đầu'}
            </span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Số tài khoản đang theo dõi:</span>
            <span className="font-bold text-stone-900">{status?.acb?.accountMasked || 'Chưa cấu hình'}</span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Thông báo đang xếp hàng gửi:</span>
            <span className="font-bold text-stone-900">
              {status?.notifications?.total.pending ?? status?.webhooks?.pending ?? 0}
            </span>
          </div>
          <div className="py-2.5 flex items-center justify-between">
            <span className="text-stone-500">Thông báo gửi không thành công:</span>
            <span className="font-bold text-stone-900">
              {status?.notifications?.total.deadLetter ?? status?.webhooks?.deadLetter ?? 0}
            </span>
          </div>
        </div>
      </div>
    </div>
  );
};
