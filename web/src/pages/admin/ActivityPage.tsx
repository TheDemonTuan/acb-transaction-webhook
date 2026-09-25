import React, { useState, useRef, useEffect } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useSearchParams } from 'react-router-dom';
import {
  RotateCcw,
  Send,
  ShieldAlert,
  RefreshCw,
  Clock,
  Smartphone,
  Globe,
  Play,
} from 'lucide-react';
import {
  fetchAuditLogs,
  fetchDeliveries,
  fetchPollRuns,
  replayDelivery,
} from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { getDeliveryStatus, getPollStatus } from '../../content/status-copy';
import { useCursorPagination, PaginationControls } from '../../shared/ui/PaginationControls';

export const ActivityPage: React.FC = () => {
  const queryClient = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();
  const [replayingId, setReplayingId] = useState<string | null>(null);
  const [actionNotice, setActionNotice] = useState<string | null>(null);
  const tabParam = searchParams.get('tab');
  const activeTab: 'polling' | 'deliveries' | 'audit' =
    tabParam === 'deliveries' ? 'deliveries' : tabParam === 'audit' ? 'audit' : 'polling';

  const pagination = useCursorPagination(20);
  const resetPagination = pagination.reset;

  // Reset pagination when active tab changes
  const prevTabRef = useRef(activeTab);
  useEffect(() => {
    if (prevTabRef.current !== activeTab) {
      prevTabRef.current = activeTab;
      resetPagination();
    }
  }, [activeTab, resetPagination]);

  const switchTab = (tab: 'polling' | 'deliveries' | 'audit') => {
    setSearchParams({ tab });
  };

  const {
    data: pollData,
    isLoading: loadingPolls,
    isFetching: fetchingPolls,
    isError: isErrorPolls,
    error: pollError,
    refetch: refetchPolls,
  } = useQuery({
    queryKey: queryKeys.pollRuns({ limit: pagination.pageSize, cursor: pagination.cursor }),
    queryFn: () => fetchPollRuns({ limit: pagination.pageSize, cursor: pagination.cursor }),
    enabled: activeTab === 'polling',
  });

  const {
    data: deliveryData,
    isLoading: loadingDeliveries,
    isFetching: fetchingDeliveries,
    isError: isErrorDeliveries,
    error: deliveryError,
    refetch: refetchDeliveries,
  } = useQuery({
    queryKey: queryKeys.deliveries({ limit: pagination.pageSize, cursor: pagination.cursor }),
    queryFn: () => fetchDeliveries({ limit: pagination.pageSize, cursor: pagination.cursor }),
    enabled: activeTab === 'deliveries',
  });

  const {
    data: auditData,
    isLoading: loadingAudit,
    isFetching: fetchingAudit,
    isError: isErrorAudit,
    error: auditError,
    refetch: refetchAudit,
  } = useQuery({
    queryKey: queryKeys.auditLogs({ limit: pagination.pageSize, cursor: pagination.cursor }),
    queryFn: () => fetchAuditLogs({ limit: pagination.pageSize, cursor: pagination.cursor }),
    enabled: activeTab === 'audit',
  });

  const handleReplay = async (deliveryId: string) => {
    setReplayingId(deliveryId);
    setActionNotice(null);
    try {
      await replayDelivery(deliveryId);
      setActionNotice('Đã đưa lượt phân phối trở lại hàng đợi gửi (PENDING).');
      queryClient.invalidateQueries({ queryKey: queryKeys.deliveries() });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
    } catch (err) {
      setActionNotice(`Lỗi: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setReplayingId(null);
    }
  };

  const polls = pollData?.items || [];
  const deliveries = deliveryData?.items || [];
  const audits = auditData?.items || [];

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h2 className="text-2xl font-bold tracking-tight text-stone-900">Hoạt động</h2>
          <p className="text-sm text-stone-500 mt-0.5">
            Lịch sử chu kỳ cập nhật, phân phối webhook và nhật ký hệ thống
          </p>
        </div>
        <button
          type="button"
          onClick={() => {
            if (activeTab === 'polling') refetchPolls();
            else if (activeTab === 'deliveries') refetchDeliveries();
            else if (activeTab === 'audit') refetchAudit();
          }}
          className="inline-flex items-center self-start sm:self-auto gap-2 px-3 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer"
        >
          <RefreshCw className="w-3.5 h-3.5" />
          <span>Làm mới</span>
        </button>
      </div>

      {/* Navigation Sub-tabs */}
      <div className="flex items-center gap-2 border-b border-stone-200 pb-2">
        <button
          type="button"
          onClick={() => switchTab('polling')}
          className={`inline-flex items-center gap-2 px-4 py-2 rounded-xl text-xs font-semibold transition cursor-pointer ${
            activeTab === 'polling'
              ? 'bg-stone-900 text-white shadow-xs'
              : 'bg-white text-stone-600 hover:bg-stone-100 border border-stone-200/80'
          }`}
        >
          <RotateCcw className="w-3.5 h-3.5" />
          <span>Chu kỳ cập nhật</span>
        </button>

        <button
          type="button"
          onClick={() => switchTab('deliveries')}
          className={`inline-flex items-center gap-2 px-4 py-2 rounded-xl text-xs font-semibold transition cursor-pointer ${
            activeTab === 'deliveries'
              ? 'bg-stone-900 text-white shadow-xs'
              : 'bg-white text-stone-600 hover:bg-stone-100 border border-stone-200/80'
          }`}
        >
          <Send className="w-3.5 h-3.5" />
          <span>Lịch sử gửi</span>
        </button>

        <button
          type="button"
          onClick={() => switchTab('audit')}
          className={`inline-flex items-center gap-2 px-4 py-2 rounded-xl text-xs font-semibold transition cursor-pointer ${
            activeTab === 'audit'
              ? 'bg-stone-900 text-white shadow-xs'
              : 'bg-white text-stone-600 hover:bg-stone-100 border border-stone-200/80'
          }`}
        >
          <ShieldAlert className="w-3.5 h-3.5" />
          <span>Nhật ký kiểm toán</span>
        </button>
      </div>

      {/* TAB 1: Polling Runs */}
      {activeTab === 'polling' && (
        <div className="bg-white rounded-2xl border border-stone-200 shadow-2xs overflow-hidden">
          <div className="px-6 py-4 border-b border-stone-100 flex items-center justify-between">
            <h3 className="font-bold text-stone-900 text-base">Chu kỳ Polling</h3>
            <span className="text-xs text-stone-500">{polls.length} lượt chạy trang này</span>
          </div>

          {loadingPolls ? (
            <div className="p-12 text-center text-xs text-stone-500">
              <RefreshCw className="w-5 h-5 animate-spin mx-auto mb-2 text-stone-400" />
              Đang tải dữ liệu...
            </div>
          ) : isErrorPolls ? (
            <div className="p-12 text-center text-xs text-rose-600 space-y-2">
              <p>Không thể tải dữ liệu: {pollError instanceof Error ? pollError.message : 'Lỗi kết nối'}</p>
              <button
                type="button"
                onClick={() => refetchPolls()}
                className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-semibold bg-white border border-rose-200 text-rose-700 hover:bg-rose-50 transition cursor-pointer shadow-2xs"
              >
                <RefreshCw className="w-3.5 h-3.5" />
                <span>Thử lại</span>
              </button>
            </div>
          ) : polls.length === 0 ? (
            <div className="p-12 text-center text-xs text-stone-500">
              Chưa có chu kỳ polling nào được ghi nhận.
            </div>
          ) : (
            <div className="divide-y divide-stone-100">
              {polls.map((p) => {
                const pollStatus = getPollStatus(p.status);
                return (
                  <div
                    key={p.id}
                    className="p-4 sm:px-6 flex flex-col sm:flex-row sm:items-center justify-between gap-3 hover:bg-stone-50/50 transition"
                  >
                    <div>
                      <div className="flex items-center gap-2">
                        <span
                          className={`text-xs font-semibold px-2 py-0.5 rounded-md border ${
                            pollStatus.tone === 'success'
                              ? 'bg-emerald-50 text-emerald-700 border-emerald-200'
                              : pollStatus.tone === 'warning'
                              ? 'bg-amber-50 text-amber-700 border-amber-200'
                              : 'bg-rose-50 text-rose-700 border-rose-200'
                          }`}
                        >
                          {pollStatus.label}
                        </span>
                        {p.classifier && (
                          <span className="text-[10px] font-medium px-1.5 py-0.5 rounded bg-stone-100 text-stone-600 border border-stone-200 uppercase tracking-wider">
                            {p.classifier}
                          </span>
                        )}
                        <span className="text-xs text-stone-500 flex items-center gap-1 font-mono">
                          <Clock className="w-3 h-3" />
                          {new Date(p.startedAt).toLocaleString('vi-VN')}
                        </span>
                      </div>
                      {p.error && (
                        <p className="text-xs text-rose-600 font-mono mt-0.5">{p.error}</p>
                      )}
                      {p.status === 'SUCCEEDED' && p.pages === 0 && p.rowsSeen === 0 && (
                        <div className="mt-1 text-xs text-stone-500">
                          <p className="font-medium">Chưa quét lịch sử giao dịch</p>
                          <p>Lượt này không quét lịch sử; giữ phiên thành công không có nghĩa là đã đồng bộ giao dịch.</p>
                        </div>
                      )}
                    </div>

                    <div className="flex items-center gap-4 text-xs text-stone-600 font-mono">
                      <span>Trang: {p.pages}</span>
                      <span>Số dòng quét: {p.rowsSeen}</span>
                    </div>
                  </div>
                );
              })}
            </div>
          )}

          {(polls.length > 0 || pagination.hasPrev) && (
            <PaginationControls
              pageNumber={pagination.pageNumber}
              itemCount={polls.length}
              pageSize={pagination.pageSize}
              hasNext={Boolean(pollData?.nextCursor)}
              hasPrev={pagination.hasPrev}
              isLoading={loadingPolls || fetchingPolls}
              onNext={() => pagination.handleNext(pollData?.nextCursor)}
              onPrev={pagination.handlePrev}
              onFirst={pagination.handleFirst}
              onPageSizeChange={pagination.setPageSize}
            />
          )}
        </div>
      )}

      {/* TAB 2: Deliveries */}
      {activeTab === 'deliveries' && (
        <div className="bg-white rounded-2xl border border-stone-200 shadow-2xs overflow-hidden">
          <div className="px-6 py-4 border-b border-stone-100 flex items-center justify-between">
            <h3 className="font-bold text-stone-900 text-base">Phân phối thông báo</h3>
            <span className="text-xs text-stone-500">{deliveries.length} lượt gửi trang này</span>
          </div>

          {actionNotice && (
            <div className="mx-6 mt-4 p-3 rounded-xl bg-stone-100 border border-stone-200 text-xs text-stone-800 flex items-center gap-2">
              <span>{actionNotice}</span>
            </div>
          )}

          {loadingDeliveries ? (
            <div className="p-12 text-center text-xs text-stone-500">
              <RefreshCw className="w-5 h-5 animate-spin mx-auto mb-2 text-stone-400" />
              Đang tải dữ liệu...
            </div>
          ) : isErrorDeliveries ? (
            <div className="p-12 text-center text-xs text-rose-600 space-y-2">
              <p>Không thể tải dữ liệu: {deliveryError instanceof Error ? deliveryError.message : 'Lỗi kết nối'}</p>
              <button
                type="button"
                onClick={() => refetchDeliveries()}
                className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-semibold bg-white border border-rose-200 text-rose-700 hover:bg-rose-50 transition cursor-pointer shadow-2xs"
              >
                <RefreshCw className="w-3.5 h-3.5" />
                <span>Thử lại</span>
              </button>
            </div>
          ) : deliveries.length === 0 ? (
            <div className="p-12 text-center text-xs text-stone-500">
              Chưa có bản ghi phân phối thông báo nào.
            </div>
          ) : (
            <div className="divide-y divide-stone-100">
              {deliveries.map((d) => {
                const deliveryStatus = getDeliveryStatus(d.status);
                const isBark = d.provider === 'BARK';

                return (
                  <div
                    key={d.id}
                    className="p-4 sm:px-6 flex flex-col sm:flex-row sm:items-center justify-between gap-3 hover:bg-stone-50/50 transition"
                  >
                    <div className="space-y-1.5">
                      <div className="flex flex-wrap items-center gap-2">
                        {/* Status badge */}
                        <span
                          className={`text-xs font-semibold px-2 py-0.5 rounded-md border ${
                            deliveryStatus.tone === 'success'
                              ? 'bg-emerald-50 text-emerald-700 border-emerald-200'
                              : deliveryStatus.tone === 'warning'
                              ? 'bg-amber-50 text-amber-700 border-amber-200'
                              : 'bg-rose-50 text-rose-700 border-rose-200'
                          }`}
                        >
                          {deliveryStatus.label}
                        </span>

                        {/* Provider badge */}
                        <span
                          className={`text-2xs font-semibold px-2 py-0.5 rounded-md border flex items-center gap-1 ${
                            isBark
                              ? 'bg-stone-50 text-stone-700 border-stone-200'
                              : 'bg-blue-50 text-blue-700 border-blue-200'
                          }`}
                        >
                          {isBark ? <Smartphone className="w-3 h-3" /> : <Globe className="w-3 h-3" />}
                          <span>{isBark ? 'Bark • iPhone' : 'Webhook'}</span>
                        </span>

                        <span className="text-xs text-stone-500 font-mono">
                          Lần gửi: {d.attempts}
                        </span>
                      </div>

                      <div className="text-xs text-stone-500">
                        Kênh nhận:{' '}
                        <span className="font-semibold text-stone-700">
                          {d.endpointName || d.endpointId}
                        </span>{' '}
                        &middot; Mã sự kiện: <span className="font-mono text-stone-600">{d.eventId}</span>
                      </div>
                    </div>

                    <div className="flex items-center gap-3">
                      <div className="text-xs text-stone-400 font-mono text-right">
                        {new Date(d.createdAt).toLocaleString('vi-VN')}
                      </div>

                      {d.status === 'DEAD_LETTER' && (
                        <button
                          type="button"
                          onClick={() => handleReplay(d.id)}
                          disabled={replayingId === d.id}
                          className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 disabled:opacity-50 transition shadow-2xs cursor-pointer"
                          title="Đưa lại vào hàng đợi gửi"
                        >
                          <Play className={`w-3 h-3 ${replayingId === d.id ? 'animate-spin' : ''}`} />
                          <span>{replayingId === d.id ? 'Đang gửi...' : 'Gửi lại'}</span>
                        </button>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          )}

          {(deliveries.length > 0 || pagination.hasPrev) && (
            <PaginationControls
              pageNumber={pagination.pageNumber}
              itemCount={deliveries.length}
              pageSize={pagination.pageSize}
              hasNext={Boolean(deliveryData?.nextCursor)}
              hasPrev={pagination.hasPrev}
              isLoading={loadingDeliveries || fetchingDeliveries}
              onNext={() => pagination.handleNext(deliveryData?.nextCursor)}
              onPrev={pagination.handlePrev}
              onFirst={pagination.handleFirst}
              onPageSizeChange={pagination.setPageSize}
            />
          )}
        </div>
      )}

      {/* TAB 3: Audit Log */}
      {activeTab === 'audit' && (
        <div className="bg-white rounded-2xl border border-stone-200 shadow-2xs overflow-hidden">
          <div className="px-6 py-4 border-b border-stone-100 flex items-center justify-between">
            <h3 className="font-bold text-stone-900 text-base">Audit Logs</h3>
            <span className="text-xs text-stone-500">{audits.length} bản ghi trang này</span>
          </div>

          {loadingAudit ? (
            <div className="p-12 text-center text-xs text-stone-500">
              <RefreshCw className="w-5 h-5 animate-spin mx-auto mb-2 text-stone-400" />
              Đang tải dữ liệu...
            </div>
          ) : isErrorAudit ? (
            <div className="p-12 text-center text-xs text-rose-600 space-y-2">
              <p>Không thể tải dữ liệu: {auditError instanceof Error ? auditError.message : 'Lỗi kết nối'}</p>
              <button
                type="button"
                onClick={() => refetchAudit()}
                className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-xl text-xs font-semibold bg-white border border-rose-200 text-rose-700 hover:bg-rose-50 transition cursor-pointer shadow-2xs"
              >
                <RefreshCw className="w-3.5 h-3.5" />
                <span>Thử lại</span>
              </button>
            </div>
          ) : audits.length === 0 ? (
            <div className="p-12 text-center text-xs text-stone-500">
              Chưa có bản ghi audit nào.
            </div>
          ) : (
            <div className="divide-y divide-stone-100">
              {audits.map((a) => (
                <div
                  key={a.id}
                  className="p-4 sm:px-6 flex flex-col sm:flex-row sm:items-center justify-between gap-3 hover:bg-stone-50/50 transition"
                >
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="font-semibold text-stone-800 text-xs">{a.action}</span>
                      <span className="text-xs text-stone-400 font-mono">
                        {a.role} &middot; {a.subject}
                      </span>
                    </div>
                    <div className="text-xs text-stone-500 font-mono mt-0.5">
                      Mục tiêu: {a.target}
                    </div>
                  </div>
                  <span className="text-xs text-stone-400 font-mono flex items-center gap-1 shrink-0">
                    <Clock className="w-3 h-3" />
                    {new Date(a.createdAt).toLocaleString('vi-VN')}
                  </span>
                </div>
              ))}
            </div>
          )}

          {(audits.length > 0 || pagination.hasPrev) && (
            <PaginationControls
              pageNumber={pagination.pageNumber}
              itemCount={audits.length}
              pageSize={pagination.pageSize}
              hasNext={Boolean(auditData?.nextCursor)}
              hasPrev={pagination.hasPrev}
              isLoading={loadingAudit || fetchingAudit}
              onNext={() => pagination.handleNext(auditData?.nextCursor)}
              onPrev={pagination.handlePrev}
              onFirst={pagination.handleFirst}
              onPageSizeChange={pagination.setPageSize}
            />
          )}
        </div>
      )}
    </div>
  );
};
