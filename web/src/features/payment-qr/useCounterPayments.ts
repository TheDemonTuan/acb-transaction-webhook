import { useCallback, useEffect, useRef, useState } from 'react';
import { useQueries, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../../api';
import { cancelPaymentOrder, createPaymentOrder, createPublicPaymentOrder, fetchPaymentOrder, fetchPublicPaymentOrder } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { useRealtimeContext } from '../../realtime/RealtimeProvider';
import type { BankTransactionCreditData } from '../../realtime/realtime.types';
import { creditMatchesPaymentOrder, isTerminalPaymentOrder, loadPaymentOrderSlots, savePaymentOrderSlots, parseCounterAmountVnd, MAX_PAYMENT_ORDER_SLOTS, PAYMENT_ORDER_SLOTS_STORAGE_KEY, type PaymentConfig, type PaymentOrder, type PaymentOrderSlot } from './payment-orders';

const orderKey = (id: string, isPublic: boolean) => isPublic ? queryKeys.publicPaymentOrder(id) : queryKeys.paymentOrder(id);

export function useCounterPayments(isPublic: boolean) {
  const client = useQueryClient();
  const { subscribe, status, reconnectCount } = useRealtimeContext();
  const [slots, setSlots] = useState(loadPaymentOrderSlots);
  const slotsRef = useRef(slots);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);
  const activeCreates = useRef(new Set<string>());
  const activeCancels = useRef(new Set<string>());
  const [creatingKeys, setCreatingKeys] = useState<string[]>([]);
  const [slotErrors, setSlotErrors] = useState<Record<string, string>>({});
  const [notice, setNotice] = useState('');
  // Fresh snapshots are required on mount, even when a previous page cached a QR.
  const snapshots = useQueries({ queries: slots.map((slot) => ({
    queryKey: orderKey(slot.orderId ?? slot.slotId, isPublic),
    queryFn: () => isPublic ? fetchPublicPaymentOrder(slot.orderId!) : fetchPaymentOrder(slot.orderId!),
    enabled: !!slot.orderId,
    refetchOnMount: 'always' as const,
    refetchInterval: (query: { state: { data?: PaymentOrder } }) => isTerminalPaymentOrder(query.state.data?.status ?? 'CREATING') ? false : 5_000,
    retry: false,
  })) });
  const orders: Record<string, PaymentOrder | undefined> = {};
  slots.forEach((slot, index) => { orders[slot.slotId] = snapshots[index]?.isFetchedAfterMount ? snapshots[index].data : undefined; });
  const updateSlots = (next: PaymentOrderSlot[]) => {
    savePaymentOrderSlots(next);
    slotsRef.current = next;
    setSlots(next);
  };
  const refreshOrders = useCallback(() => {
    for (const slot of slotsRef.current) if (slot.orderId) void client.invalidateQueries({ queryKey: orderKey(slot.orderId, isPublic) });
  }, [client, isPublic]);
  useEffect(() => {
    const storage = (event: StorageEvent) => {
      if (event.key !== PAYMENT_ORDER_SLOTS_STORAGE_KEY && event.key !== null) return;
      const next = loadPaymentOrderSlots(); slotsRef.current = next; setSlots(next);
    };
    window.addEventListener('storage', storage);
    window.addEventListener('pageshow', refreshOrders);
    window.addEventListener('focus', refreshOrders);
    window.addEventListener('online', refreshOrders);
    return () => {
      window.removeEventListener('storage', storage);
      window.removeEventListener('pageshow', refreshOrders);
      window.removeEventListener('focus', refreshOrders);
      window.removeEventListener('online', refreshOrders);
    };
  }, [refreshOrders]);
  useEffect(() => { if (status === 'CONNECTED') refreshOrders(); }, [status, reconnectCount, refreshOrders]);
  useEffect(() => subscribe<BankTransactionCreditData>('bank.transaction.credit', ({ data }) => {
    // SePay never confirms or refreshes a payOS intent, including missing snapshots.
    if (data?.provider !== 'PAYOS') return;
    for (const slot of slotsRef.current) {
      if (!slot.orderId) continue;
      const snapshot = client.getQueryData<PaymentOrder>(orderKey(slot.orderId, isPublic));
      if (!snapshot || creditMatchesPaymentOrder(snapshot, data)) void client.invalidateQueries({ queryKey: orderKey(slot.orderId, isPublic) });
    }
  }), [subscribe, client, isPublic]);
  const postSlot = async (slot: PaymentOrderSlot) => {
    if (activeCreates.current.has(slot.idempotencyKey) || slot.orderId) return;
    activeCreates.current.add(slot.idempotencyKey); setCreatingKeys([...activeCreates.current]);
    setSlotErrors((previous) => ({ ...previous, [slot.slotId]: '' }));
    try {
      const order = isPublic ? await createPublicPaymentOrder(slot.amountVnd, 'OPERATOR_DYNAMIC', slot.idempotencyKey) : await createPaymentOrder(slot.amountVnd, slot.idempotencyKey);
      // Navigation may mount a newer tray while this request is still in flight.
      // Leave this persisted intent intact for same-key retry after unmount.
      if (!mounted.current) return;
      client.setQueryData(orderKey(order.id, isPublic), order);
      if (slotsRef.current.some((item) => item.slotId === slot.slotId)) updateSlots(slotsRef.current.map((item) => item.slotId === slot.slotId ? { ...item, orderId: order.id } : item));
    } catch {
      setSlotErrors((previous) => ({ ...previous, [slot.slotId]: 'Đang chờ xác nhận. Giữ thẻ và thử lại cùng đơn, không đổi khóa tạo đơn.' }));
    } finally { activeCreates.current.delete(slot.idempotencyKey); setCreatingKeys([...activeCreates.current]); }
  };
  const addSlot = (amountInput: string, nameInput: string): PaymentOrderSlot | null => {
    const config = client.getQueryData<PaymentConfig>(queryKeys.paymentConfig);
    const amountVnd = parseCounterAmountVnd(amountInput, config?.maxAmountVnd);
    if (amountVnd === null) { setNotice('Nhập số nguyên dương, chỉ chữ số (đơn vị nghìn đồng).'); return null; }
    if (!config?.ready || !navigator.onLine) { setNotice('payOS chưa sẵn sàng nhận đơn mới.'); return null; }
    if (slotsRef.current.length >= MAX_PAYMENT_ORDER_SLOTS) { setNotice('Khay đang đủ 3 khách'); return null; }
    try {
      const number = [1, 2, 3].find((value) => !slotsRef.current.some((item) => item.name === `Khách ${value}`)) ?? slotsRef.current.length + 1;
      const slot: PaymentOrderSlot = { slotId: crypto.randomUUID(), name: nameInput.trim() || `Khách ${number}`, amountVnd, idempotencyKey: crypto.randomUUID() };
      updateSlots([...slotsRef.current, slot]);
      setNotice(''); void postSlot(slot); return slot;
    } catch { setNotice('Không thể lưu đơn trên thiết bị. Chưa gửi yêu cầu tạo đơn; hãy bật lưu trữ trình duyệt.'); return null; }
  };
  const removeSlot = (slotId: string) => {
    try { updateSlots(slotsRef.current.filter((slot) => slot.slotId !== slotId)); }
    catch { setNotice('Không thể lưu thay đổi khay khách.'); }
  };
  const cancelSlot = async (slotId: string) => {
    const slot = slotsRef.current.find((item) => item.slotId === slotId);
    if (isPublic || !slot?.orderId || activeCancels.current.has(slotId)) return;
    activeCancels.current.add(slotId);
    setSlotErrors((previous) => ({ ...previous, [slotId]: '' }));
    try { client.setQueryData(orderKey(slot.orderId, isPublic), await cancelPaymentOrder(slot.orderId)); }
    catch (error) {
      setSlotErrors((previous) => ({ ...previous, [slotId]: error instanceof ApiError && error.status === 403 ? 'Chỉ chủ sở hữu hoặc người vận hành được hủy đơn.' : 'Đang chờ xác nhận hủy. Giữ nguyên đơn và kiểm tra lại trạng thái.' }));
      void client.invalidateQueries({ queryKey: orderKey(slot.orderId, isPublic) });
    } finally { activeCancels.current.delete(slotId); }
  };
  const errors = { ...slotErrors };
  slots.forEach((slot, index) => { if (snapshots[index]?.isError && !errors[slot.slotId]) errors[slot.slotId] = 'Đang mất cập nhật trạng thái. Giữ nguyên đơn và QR chưa hết hạn.'; });
  return { slots, orders, creatingKeys, slotErrors: errors, notice, addSlot, retrySlot: (slot: PaymentOrderSlot) => { void postSlot(slot); }, removeSlot, cancelSlot, refreshOrders };
}
