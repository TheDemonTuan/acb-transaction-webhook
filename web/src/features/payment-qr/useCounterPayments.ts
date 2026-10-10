import { useCallback, useEffect, useRef, useState } from 'react';
import { useQueries, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../../api';
import { cancelPaymentOrder, createPaymentOrder, createPublicPaymentOrder, fetchPaymentOrder, fetchPublicPaymentOrder } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { useRealtimeContext } from '../../realtime/RealtimeProvider';
import type { BankTransactionCreditData } from '../../realtime/realtime.types';
import { archivePaymentOrderSlot, attachPaymentOrder, creditMatchesPaymentOrder, isTerminalPaymentOrder, loadPaymentOrderTray, mergeLegacyPaymentOrderSlots, readPaymentOrderTray, savePaymentOrderTray, showPaymentOrderSlot, parseCounterAmountVnd, PAYMENT_ORDER_SLOTS_STORAGE_KEY, PAYMENT_ORDER_TRAY_STORAGE_KEY, type PaymentConfig, type PaymentOrder, type PaymentOrderSlot, type PaymentOrderTray } from './payment-orders';

const orderKey = (id: string, isPublic: boolean) => isPublic ? queryKeys.publicPaymentOrder(id) : queryKeys.paymentOrder(id);
export const COUNTER_ARCHIVE_PAGE_SIZE = 8;

export function useCounterPayments(isPublic: boolean, selectedSlotId?: string, archivePage: number | null = null) {
  const client = useQueryClient();
  const { subscribe, status, reconnectCount } = useRealtimeContext();
  const [tray, setTray] = useState(loadPaymentOrderTray);
  const trayRef = useRef(tray);
  const mounted = useRef(true);
  const activeCreates = useRef(new Set<string>());
  const activeCancels = useRef(new Set<string>());
  const recoveryOffset = useRef(0);
  const recoveryRunning = useRef(false);
  const [creatingKeys, setCreatingKeys] = useState<string[]>([]);
  const [slotErrors, setSlotErrors] = useState<Record<string, string>>({});
  const [notice, setNotice] = useState('');
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);

  // Serialize read/modify/write across tabs. Each mutation re-reads the durable
  // tray, including delayed responses from a component that has since unmounted.
  const mutateTray = useCallback(async (change: (current: PaymentOrderTray) => PaymentOrderTray) => {
    const commit = () => {
      const current = readPaymentOrderTray();
      const next = change(current);
      // Reads may already include a legacy-tab migration; persist that merged
      // state even when this mutation itself makes no further change.
      savePaymentOrderTray(next);
      trayRef.current = next;
      if (mounted.current) setTray(next);
      return next;
    };
    if (navigator.locks) return navigator.locks.request(PAYMENT_ORDER_TRAY_STORAGE_KEY, commit);
    return commit();
  }, []);

  const archivedPage = archivePage === null ? [] : [...tray.archived].reverse().slice(archivePage * COUNTER_ARCHIVE_PAGE_SIZE, (archivePage + 1) * COUNTER_ARCHIVE_PAGE_SIZE);
  const allSlots = [...tray.visible, ...tray.archived];
  const watched = [...tray.visible];
  for (const slot of [...archivedPage, ...allSlots.filter((item) => item.slotId === selectedSlotId)]) {
    if (!watched.some((item) => item.slotId === slot.slotId)) watched.push(slot);
  }
  const watchedRef = useRef(watched);
  watchedRef.current = watched;
  // At most three recent, one selected archived, and eight drawer entries poll.
  // Terminal snapshots never poll; hidden archives have no query observers.
  const snapshots = useQueries({ queries: watched.map((slot) => ({
    queryKey: orderKey(slot.orderId ?? slot.slotId, isPublic),
    queryFn: () => isPublic ? fetchPublicPaymentOrder(slot.orderId!) : fetchPaymentOrder(slot.orderId!),
    enabled: !!slot.orderId,
    refetchOnMount: 'always' as const,
    refetchInterval: (query: { state: { data?: PaymentOrder } }) => isTerminalPaymentOrder(query.state.data?.status ?? 'CREATING') ? false : 5_000,
    retry: false,
  })) });
  const orders: Record<string, PaymentOrder | undefined> = {};
  const errors = { ...slotErrors };
  const fresh: Array<{ slotId: string; order: PaymentOrder }> = [];
  watched.forEach((slot, index) => {
    const snapshot = snapshots[index];
    orders[slot.slotId] = snapshot?.isFetchedAfterMount ? snapshot.data : undefined;
    if (snapshot?.isError && !errors[slot.slotId]) errors[slot.slotId] = 'Đang mất cập nhật trạng thái. Giữ nguyên đơn và QR chưa hết hạn.';
    if (snapshot?.isFetchedAfterMount && snapshot.data && (slot.orderCode !== snapshot.data.orderCode || slot.status !== snapshot.data.status)) fresh.push({ slotId: slot.slotId, order: snapshot.data });
  });
  const metadataSignature = fresh.map(({ slotId, order }) => `${slotId}:${order.id}:${order.status}:${order.orderCode}`).join('|');
  useEffect(() => {
    if (!metadataSignature) return;
    void mutateTray((current) => fresh.reduce((next, { slotId, order }) => attachPaymentOrder(next, slotId, order), current)).catch(() => {
      if (mounted.current) setNotice('Không thể lưu trạng thái đơn. Đơn và khóa tạo đơn vẫn được giữ nguyên.');
    });
    // Snapshot metadata, not QR/account data, is the effect's durable input.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [metadataSignature, mutateTray]);

  const fetchSlot = useCallback(async (slot: PaymentOrderSlot) => {
    if (!slot.orderId) return;
    try {
      const order = await client.fetchQuery({ queryKey: orderKey(slot.orderId, isPublic), queryFn: () => isPublic ? fetchPublicPaymentOrder(slot.orderId!) : fetchPaymentOrder(slot.orderId!), staleTime: 0, retry: false });
      await mutateTray((current) => attachPaymentOrder(current, slot.slotId, order));
    } catch { /* Keep the recoverable intent on network/storage failure. */ }
  }, [client, isPublic, mutateTray]);
  const refreshOrders = useCallback(() => {
    for (const slot of watchedRef.current) if (slot.orderId) void client.invalidateQueries({ queryKey: orderKey(slot.orderId, isPublic) });
    if (recoveryRunning.current) return;
    // One bounded rotating recovery batch per mount/focus/reconnect/manual refresh,
    // not an ever-growing interval. Known archived order codes also recover on SSE.
    const unresolved = trayRef.current.archived.filter((slot) => slot.orderId && (!slot.status || !isTerminalPaymentOrder(slot.status)) && !watchedRef.current.some((item) => item.slotId === slot.slotId));
    if (!unresolved.length) return;
    const start = recoveryOffset.current % unresolved.length;
    const batch = [...unresolved.slice(start), ...unresolved.slice(0, start)].slice(0, COUNTER_ARCHIVE_PAGE_SIZE);
    recoveryOffset.current = start + batch.length;
    recoveryRunning.current = true;
    void Promise.all(batch.map(fetchSlot)).finally(() => { recoveryRunning.current = false; });
  }, [client, isPublic, fetchSlot]);
  useEffect(() => {
    const storage = (event: StorageEvent) => {
      if (event.key === PAYMENT_ORDER_SLOTS_STORAGE_KEY) {
        void mutateTray((current) => {
          const raw = localStorage.getItem(PAYMENT_ORDER_SLOTS_STORAGE_KEY);
          if (!raw) return current;
          try {
            const legacy: unknown = JSON.parse(raw);
            return Array.isArray(legacy) ? mergeLegacyPaymentOrderSlots(current, legacy) : current;
          } catch {
            return current;
          }
        }).catch(() => { if (mounted.current) setNotice('Không thể đồng bộ đơn từ phiên cũ. Giữ nguyên phiên đó và thử lại khi bộ nhớ thiết bị sẵn sàng.'); });
        return;
      }
      if (event.key !== PAYMENT_ORDER_TRAY_STORAGE_KEY && event.key !== null) return;
      try { const next = readPaymentOrderTray(); trayRef.current = next; setTray(next); }
      catch { setNotice('Không đọc được khay đơn đã lưu. Chưa gửi đơn mới để tránh mất dữ liệu.'); }
    };
    window.addEventListener('storage', storage);
    window.addEventListener('pageshow', refreshOrders);
    window.addEventListener('focus', refreshOrders);
    window.addEventListener('online', refreshOrders);
    refreshOrders();
    return () => {
      window.removeEventListener('storage', storage);
      window.removeEventListener('pageshow', refreshOrders);
      window.removeEventListener('focus', refreshOrders);
      window.removeEventListener('online', refreshOrders);
    };
  }, [refreshOrders]);
  useEffect(() => { if (status === 'CONNECTED') refreshOrders(); }, [status, reconnectCount, refreshOrders]);
  useEffect(() => subscribe<BankTransactionCreditData>('bank.transaction.credit', ({ data }) => {
    if (data?.provider !== 'PAYOS') return;
    for (const slot of [...trayRef.current.visible, ...trayRef.current.archived]) {
      if (!slot.orderId) continue;
      const snapshot = client.getQueryData<PaymentOrder>(orderKey(slot.orderId, isPublic));
      const orderCode = slot.orderCode ?? snapshot?.orderCode;
      if (orderCode && creditMatchesPaymentOrder({ orderCode }, data)) void fetchSlot(slot);
      else if (!orderCode && watchedRef.current.some((item) => item.slotId === slot.slotId)) void client.invalidateQueries({ queryKey: orderKey(slot.orderId, isPublic) });
    }
  }), [subscribe, client, isPublic, fetchSlot]);

  const postSlot = async (slot: PaymentOrderSlot) => {
    if (activeCreates.current.has(slot.idempotencyKey) || slot.orderId) return;
    activeCreates.current.add(slot.idempotencyKey);
    if (mounted.current) { setCreatingKeys([...activeCreates.current]); setSlotErrors((previous) => ({ ...previous, [slot.slotId]: '' })); }
    try {
      const order = isPublic ? await createPublicPaymentOrder(slot.amountVnd, 'OPERATOR_DYNAMIC', slot.idempotencyKey) : await createPaymentOrder(slot.amountVnd, slot.idempotencyKey);
      client.setQueryData(orderKey(order.id, isPublic), order);
      await mutateTray((current) => attachPaymentOrder(current, slot.slotId, order));
    } catch {
      if (mounted.current) setSlotErrors((previous) => ({ ...previous, [slot.slotId]: 'Đang chờ xác nhận. Thử lại cùng đơn; khóa và số tiền đã được giữ trong Đơn trước nếu thẻ đã lưu trữ.' }));
    } finally {
      activeCreates.current.delete(slot.idempotencyKey);
      if (mounted.current) setCreatingKeys([...activeCreates.current]);
    }
  };
  const addSlot = async (amountInput: string, nameInput: string): Promise<PaymentOrderSlot | null> => {
    const config = client.getQueryData<PaymentConfig>(queryKeys.paymentConfig);
    const amountVnd = parseCounterAmountVnd(amountInput, config?.maxAmountVnd);
    if (amountVnd === null) { setNotice('Nhập số nguyên dương, chỉ chữ số (đơn vị nghìn đồng).'); return null; }
    if (!config?.ready || !navigator.onLine) { setNotice('payOS chưa sẵn sàng nhận đơn mới.'); return null; }
    try {
      let slot: PaymentOrderSlot | undefined;
      await mutateTray((current) => {
        slot = { slotId: crypto.randomUUID(), name: nameInput.trim() || `Khách ${current.visible.length + current.archived.length + 1}`, amountVnd, idempotencyKey: crypto.randomUUID() };
        return showPaymentOrderSlot(current, slot);
      });
      setNotice('');
      void postSlot(slot!);
      return slot!;
    } catch { setNotice('Không thể lưu đơn trên thiết bị. Chưa gửi yêu cầu tạo đơn; hãy bật lưu trữ trình duyệt.'); return null; }
  };
  const removeSlot = async (slotId: string): Promise<boolean> => {
    try { await mutateTray((current) => archivePaymentOrderSlot(current, slotId)); return true; }
    catch { setNotice('Không thể lưu thay đổi khay khách.'); return false; }
  };
  const restoreSlot = (slotId: string) => {
    void mutateTray((current) => {
      const slot = current.archived.find((item) => item.slotId === slotId);
      return slot ? showPaymentOrderSlot(current, slot) : current;
    }).catch(() => setNotice('Không thể khôi phục thẻ vào khay. Đơn vẫn được giữ trong Đơn trước.'));
  };
  const cancelSlot = async (slotId: string) => {
    const slot = [...trayRef.current.visible, ...trayRef.current.archived].find((item) => item.slotId === slotId);
    if (isPublic || !slot?.orderId || activeCancels.current.has(slotId)) return;
    activeCancels.current.add(slotId);
    setSlotErrors((previous) => ({ ...previous, [slotId]: '' }));
    try {
      const order = await cancelPaymentOrder(slot.orderId);
      client.setQueryData(orderKey(slot.orderId, isPublic), order);
      await mutateTray((current) => attachPaymentOrder(current, slot.slotId, order));
    } catch (error) {
      setSlotErrors((previous) => ({ ...previous, [slotId]: error instanceof ApiError && error.status === 403 ? 'Chỉ chủ sở hữu hoặc người vận hành được hủy đơn.' : 'Đang chờ xác nhận hủy. Giữ nguyên đơn và kiểm tra lại trạng thái.' }));
      void client.invalidateQueries({ queryKey: orderKey(slot.orderId, isPublic) });
    } finally { activeCancels.current.delete(slotId); }
  };
  return { slots: tray.visible, archived: tray.archived, archivedPage, allSlots, orders, creatingKeys, slotErrors: errors, notice, addSlot, retrySlot: (slot: PaymentOrderSlot) => { void postSlot(slot); }, removeSlot, restoreSlot, cancelSlot, refreshOrders };
}
