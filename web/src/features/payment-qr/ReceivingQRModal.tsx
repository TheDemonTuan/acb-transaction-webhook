import React, { useCallback, useEffect, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { QrCode, X, Copy, Check, Wifi, Edit3, Eye, EyeOff, Plus, RefreshCw } from 'lucide-react';
import { ApiError } from '../../api';
import { cancelPaymentOrder, createPaymentOrder, createPublicPaymentOrder, fetchPaymentConfig, fetchPaymentOrder, fetchPublicPaymentOrder, fetchTransactions } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { useRealtimeContext } from '../../realtime/RealtimeProvider';
import type { BankTransactionCreditData } from '../../realtime/realtime.types';
import { formatVndCurrency } from '../../shared/formatters/money';
import { formatDateTimeVN } from '../../shared/formatters/datetime';
import { isPublicViewerHost } from '../../app/runtime-mode';
import { creditMatchesPaymentOrder, isTerminalPaymentOrder, loadPaymentOrderSlots, savePaymentOrderSlots, parseCounterAmountVnd, MAX_PAYMENT_ORDER_SLOTS, PAYMENT_ORDER_SLOTS_STORAGE_KEY, type PaymentOrder, type PaymentOrderSlot } from './payment-orders';
import { FixedPaymentQR, PaymentQRImage } from './PaymentQRImage';
import { buildWifiQRString, generateWifiQRDataURL, loadWifiSettings, saveWifiSettings, WIFI_STORAGE_KEY, type WifiSettings } from './wifi-qr';

const statusLabels: Record<PaymentOrder['status'], string> = {
  CREATING: 'Đang tạo đơn', PENDING: 'Đang chờ thanh toán', PROCESSING: 'Đang xử lý', UNDERPAID: 'Chưa đủ số tiền',
  PAID: 'Đã thanh toán', CANCELLED: 'Đã hủy', EXPIRED: 'Đã hết hạn', FAILED: 'Không tạo được đơn',
};
const orderKey = (id: string, isPublic: boolean) => isPublic ? queryKeys.publicPaymentOrder(id) : queryKeys.paymentOrder(id);

const OrderCard: React.FC<{
  slot: PaymentOrderSlot; isPublic: boolean; isOpen: boolean; now: number; creating: boolean;
  onRetry: (slot: PaymentOrderSlot) => void; onRemove: (slotId: string) => void;
}> = ({ slot, isPublic, isOpen, now, creating, onRetry, onRemove }) => {
  const client = useQueryClient();
  const [cancelling, setCancelling] = useState(false);
  const [cancelError, setCancelError] = useState('');
  const [copyNotice, setCopyNotice] = useState('');
  const query = useQuery({
    queryKey: orderKey(slot.orderId ?? slot.slotId, isPublic),
    queryFn: () => isPublic ? fetchPublicPaymentOrder(slot.orderId!) : fetchPaymentOrder(slot.orderId!),
    enabled: isOpen && !!slot.orderId,
    refetchInterval: (q) => isTerminalPaymentOrder(q.state.data?.status ?? 'CREATING') ? false : 5_000,
    retry: false,
  });
  const order = query.data;
  const expiredLocally = !!order && Date.parse(order.expiresAt) <= now;
  const payable = !!order && !isTerminalPaymentOrder(order.status) && !expiredLocally;
  const copy = (value: string) => { void navigator.clipboard.writeText(value).then(() => setCopyNotice('Đã sao chép'), () => setCopyNotice('Không thể sao chép')); };
  const cancel = async () => {
    if (!slot.orderId || cancelling) return;
    setCancelling(true); setCancelError('');
    try {
      const result = await cancelPaymentOrder(slot.orderId);
      client.setQueryData(orderKey(slot.orderId, isPublic), result);
    } catch (error) {
      setCancelError(error instanceof ApiError && error.status === 403
        ? 'Chỉ chủ sở hữu hoặc người vận hành được hủy đơn.'
        : 'Đang chờ xác nhận. Không tạo đơn khác tự động; hãy thử lại với cùng đơn.');
      void query.refetch();
    }
    finally { setCancelling(false); }
  };
  return <article aria-label={slot.name} className={`rounded-2xl border p-4 space-y-3 bg-white ${order?.status === 'PAID' ? 'border-emerald-400 bg-emerald-50/30' : 'border-stone-200'}`}>
    <div className="flex items-start justify-between gap-2">
      <div><h4 className="font-bold text-stone-900">{slot.name}</h4><p className="text-lg font-bold text-emerald-700">{formatVndCurrency(slot.amountVnd)}</p></div>
      <button type="button" onClick={() => onRemove(slot.slotId)} className="p-1 text-stone-500 hover:text-rose-600" aria-label={`Bỏ ${slot.name} khỏi khay`} title="Chỉ bỏ khỏi khay, không hủy đơn"><X className="w-4 h-4" /></button>
    </div>
    <p role="status" className="text-sm font-semibold">{order ? statusLabels[order.status] : creating ? 'Đang tạo đơn' : 'Đang chờ xác nhận'}</p>
    {order && <p className="text-xs text-stone-500">Mã đơn: <span className="font-mono">{order.orderCode}</span></p>}
    {order?.status === 'PAID' ? <div className="text-sm text-emerald-800 font-semibold">Đã nhận {formatVndCurrency(order.amountVnd)}{order.paidAt && <p className="text-xs mt-1">{formatDateTimeVN(order.paidAt)}</p>}</div> : <>
      {payable && order.qrCode && <PaymentQRImage payload={order.qrCode} alt={`QR thanh toán ${slot.name}`} />}
      {payable && !order.qrCode && <p className="text-sm text-stone-500">Đang chờ mã QR từ payOS.</p>}
      {expiredLocally && !isTerminalPaymentOrder(order!.status) && <p className="text-sm text-amber-800">Đã hết thời gian hiển thị QR. Đang chờ xác nhận trạng thái từ máy chủ.</p>}
      {payable && <div className="space-y-2 text-xs">
        <p className="font-semibold">KienlongBank</p>
        {order.accountName && <p>{order.accountName}</p>}
        {order.accountNumber && <button type="button" onClick={() => copy(order.accountNumber!)} className="flex items-center gap-2 break-all"><span>Tài khoản nhận / VA: {order.accountNumber}</span><Copy className="w-3 h-3 shrink-0" /></button>}
        <button type="button" onClick={() => copy(String(order.amountVnd))} className="flex items-center gap-2">Sao chép số tiền VND<Copy className="w-3 h-3" /></button>
        {order.checkoutUrl && <a href={order.checkoutUrl} target="_blank" rel="noreferrer" className="block rounded-xl bg-emerald-600 text-white px-3 py-2 text-center font-bold">Thanh toán trên payOS</a>}
        {copyNotice && <p role="status">{copyNotice}</p>}
      </div>}
    </>}
    {(query.isError || cancelError) && <p role="alert" className="text-xs text-amber-800">{cancelError || 'Đang chờ xác nhận. Giữ nguyên đơn khi mất mạng.'}</p>}
    {!slot.orderId && <button type="button" disabled={creating} onClick={() => onRetry(slot)} className="text-sm font-semibold text-emerald-700">{creating ? 'Đang gửi…' : 'Thử lại cùng đơn'}</button>}
    {slot.orderId && <div className="flex flex-wrap gap-3 text-xs">
      <button type="button" onClick={() => { void query.refetch(); }} className="inline-flex gap-1 items-center text-stone-600"><RefreshCw className="w-3 h-3" />Làm mới trạng thái</button>
      {!isPublic && order && !isTerminalPaymentOrder(order.status) && <button type="button" disabled={cancelling} onClick={() => { void cancel(); }} className="text-rose-700 font-semibold">{cancelling ? 'Đang xác nhận hủy…' : 'Hủy đơn trên payOS'}</button>}
    </div>}
    {order && isTerminalPaymentOrder(order.status) && order.status !== 'PAID' && <p className="text-xs text-stone-500">Bỏ thẻ này rồi thêm khách để tạo đơn mới.</p>}
  </article>;
};

export const ReceivingQRModal: React.FC<{ isOpen: boolean; onClose: () => void }> = ({ isOpen, onClose }) => {
  const queryClient = useQueryClient();
  const isPublic = isPublicViewerHost();
  const { subscribe, status, reconnectCount, networkOnline, serverReachable, forceReconnect } = useRealtimeContext();
  const [activeModeTab, setActiveModeTab] = useState<'payment' | 'fixed' | 'wifi' | 'history'>('payment');
  const [slots, setSlots] = useState<PaymentOrderSlot[]>(loadPaymentOrderSlots);
  const slotsRef = useRef(slots);
  const activeCreates = useRef(new Set<string>());
  const [creatingKeys, setCreatingKeys] = useState<string[]>([]);
  const [amountInput, setAmountInput] = useState('');
  const [nameInput, setNameInput] = useState('');
  const [notice, setNotice] = useState('');
  const [now, setNow] = useState(Date.now());
  const [liveCredits, setLiveCredits] = useState<BankTransactionCreditData[]>([]);
  const seenCredits = useRef(new Set<string>());
  const [wifiSettings, setWifiSettings] = useState<WifiSettings>(loadWifiSettings);
  const [wifiQRDataURL, setWifiQRDataURL] = useState('');
  const [isEditingWifi, setIsEditingWifi] = useState(false);
  const [wifiEditForm, setWifiEditForm] = useState<WifiSettings>(loadWifiSettings);
  const [showWifiPassword, setShowWifiPassword] = useState(false);
  const [wifiCopiedField, setWifiCopiedField] = useState<'ssid' | 'password' | null>(null);
  const config = useQuery({ queryKey: queryKeys.paymentConfig, queryFn: fetchPaymentConfig, enabled: isOpen, refetchInterval: 30_000 });
  const today = new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Ho_Chi_Minh' }).format(new Date(now));
  const history = useQuery({ queryKey: queryKeys.transactions({ direction: 'credit', from: today, to: today, limit: 20 }), queryFn: () => fetchTransactions({ direction: 'credit', from: today, to: today, limit: 20 }), enabled: isOpen });
  const preview = parseCounterAmountVnd(amountInput, config.data?.maxAmountVnd);
  const updateSlots = (next: PaymentOrderSlot[]) => {
    savePaymentOrderSlots(next);
    slotsRef.current = next;
    setSlots(next);
  };
  const refreshOrders = useCallback(() => {
    for (const slot of slotsRef.current) if (slot.orderId) void queryClient.invalidateQueries({ queryKey: orderKey(slot.orderId, isPublic) });
  }, [queryClient, isPublic]);
  useEffect(() => {
    if (!isOpen) return;
    const restored = loadPaymentOrderSlots();
    slotsRef.current = restored; setSlots(restored);
    refreshOrders();
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    const keydown = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    window.addEventListener('keydown', keydown);
    window.addEventListener('pageshow', refreshOrders);
    window.addEventListener('focus', refreshOrders);
    window.addEventListener('online', refreshOrders);
    return () => {
      window.clearInterval(timer); window.removeEventListener('keydown', keydown);
      window.removeEventListener('pageshow', refreshOrders); window.removeEventListener('focus', refreshOrders); window.removeEventListener('online', refreshOrders);
    };
  }, [isOpen, onClose, refreshOrders]);
  useEffect(() => { if (isOpen && status === 'CONNECTED') refreshOrders(); }, [isOpen, status, reconnectCount, refreshOrders]);
  useEffect(() => {
    if (!isOpen) return;
    const creditUnsub = subscribe<BankTransactionCreditData>('bank.transaction.credit', ({ data }) => {
      if (!data) return;
      for (const slot of slotsRef.current) {
        if (!slot.orderId) continue;
        const snapshot = queryClient.getQueryData<PaymentOrder>(orderKey(slot.orderId, isPublic));
        if (!snapshot || creditMatchesPaymentOrder(snapshot, data)) void queryClient.invalidateQueries({ queryKey: orderKey(slot.orderId, isPublic) });
      }
      void queryClient.invalidateQueries({ queryKey: queryKeys.transactions() });
      if (Number(data.credit) > 0 && !seenCredits.current.has(data.transactionId)) {
        seenCredits.current.add(data.transactionId);
        setLiveCredits((previous) => [data, ...previous].slice(0, 20));
      }
    });
    return creditUnsub;
  }, [isOpen, subscribe, queryClient, isPublic]);
  useEffect(() => {
    const refreshWifi = () => {
      const current = loadWifiSettings(); setWifiSettings(current); setWifiEditForm(current);
      const payload = buildWifiQRString(current.ssid, current.password, current.security, current.hidden);
      if (payload) void generateWifiQRDataURL(payload).then(setWifiQRDataURL); else setWifiQRDataURL('');
    };
    if (isOpen) refreshWifi();
    const storage = (event: StorageEvent) => {
      if (event.key === WIFI_STORAGE_KEY) refreshWifi();
      if (event.key === PAYMENT_ORDER_SLOTS_STORAGE_KEY) { const next = loadPaymentOrderSlots(); slotsRef.current = next; setSlots(next); }
    };
    window.addEventListener('storage', storage);
    return () => window.removeEventListener('storage', storage);
  }, [isOpen]);
  const postSlot = async (slot: PaymentOrderSlot) => {
    if (activeCreates.current.has(slot.idempotencyKey)) return;
    activeCreates.current.add(slot.idempotencyKey); setCreatingKeys([...activeCreates.current]);
    try {
      const order = isPublic ? await createPublicPaymentOrder(slot.amountVnd, 'OPERATOR_DYNAMIC', slot.idempotencyKey) : await createPaymentOrder(slot.amountVnd, slot.idempotencyKey);
      queryClient.setQueryData(orderKey(order.id, isPublic), order);
      const current = slotsRef.current;
      if (current.some((item) => item.slotId === slot.slotId)) updateSlots(current.map((item) => item.slotId === slot.slotId ? { ...item, orderId: order.id } : item));
    } catch { setNotice('Đang chờ xác nhận. Giữ thẻ và thử lại cùng đơn, không đổi khóa tạo đơn.'); }
    finally { activeCreates.current.delete(slot.idempotencyKey); setCreatingKeys([...activeCreates.current]); }
  };
  const addSlot = (event: React.FormEvent) => {
    event.preventDefault();
    if (preview === null || !config.data?.ready || slotsRef.current.length >= MAX_PAYMENT_ORDER_SLOTS) return;
    try {
      const nextNumber = [1, 2, 3].find((number) => !slotsRef.current.some((item) => item.name === `Khách ${number}`)) ?? slotsRef.current.length + 1;
      const slot: PaymentOrderSlot = { slotId: crypto.randomUUID(), name: nameInput.trim() || `Khách ${nextNumber}`, amountVnd: preview, idempotencyKey: crypto.randomUUID() };
      updateSlots([...slotsRef.current, slot]);
      setAmountInput(''); setNameInput(''); setNotice('');
      void postSlot(slot);
    } catch { setNotice('Không thể lưu đơn trên thiết bị. Chưa gửi yêu cầu tạo đơn; hãy bật lưu trữ trình duyệt.'); }
  };
  const removeSlot = (id: string) => {
    try { updateSlots(slotsRef.current.filter((slot) => slot.slotId !== id)); }
    catch { setNotice('Không thể lưu thay đổi khay khách.'); }
  };
  const handleCopyWifiText = (text: string, field: 'ssid' | 'password') => {
    void navigator.clipboard.writeText(text).then(() => { setWifiCopiedField(field); window.setTimeout(() => setWifiCopiedField(null), 2_000); }, () => setNotice('Không thể sao chép thông tin WiFi.'));
  };
  const handleSaveWifi = (event: React.FormEvent) => {
    event.preventDefault(); const updated = saveWifiSettings(wifiEditForm); setWifiSettings(updated); setIsEditingWifi(false);
    const payload = buildWifiQRString(updated.ssid, updated.password, updated.security, updated.hidden);
    if (payload) void generateWifiQRDataURL(payload).then(setWifiQRDataURL); else setWifiQRDataURL('');
  };
  if (!isOpen) return null;
  return <div className="fixed inset-0 z-50 flex items-center justify-center p-2.5 sm:p-4 md:p-6 bg-stone-950/70 backdrop-blur-xs">
    <div role="dialog" aria-modal="true" aria-label="Nhận tiền payOS" className="bg-white w-full max-w-md md:max-w-5xl lg:max-w-6xl rounded-3xl shadow-2xl border border-stone-200 overflow-hidden flex flex-col max-h-[94vh]">
      <div className="px-4 py-3 sm:px-6 border-b border-stone-100 flex items-center justify-between bg-stone-50/80 shrink-0">
        <div className="flex items-center gap-3"><QrCode className="w-6 h-6 text-emerald-700" /><div><h3 className="font-bold text-sm sm:text-base">Nhận tiền payOS · KienlongBank</h3><p className="text-xs text-stone-500">Tối đa 3 khách, mỗi khách một đơn riêng</p></div></div>
        <button type="button" onClick={onClose} aria-label="Đóng mã QR" className="p-2"><X className="w-5 h-5" /></button>
      </div>
      <div className="flex gap-1 p-2 border-b border-stone-100 shrink-0 overflow-x-auto">
        {([['payment', 'Tại quầy'], ['fixed', 'QR cố định'], ['wifi', 'WiFi quán'], ['history', 'Lịch sử']] as const).map(([tab, label]) => <button type="button" key={tab} onClick={() => setActiveModeTab(tab)} className={`px-3 py-2 rounded-xl text-xs font-bold whitespace-nowrap ${activeModeTab === tab ? 'bg-emerald-100 text-emerald-800' : 'text-stone-600'}`}>{label}</button>)}
      </div>
      <div className="overflow-y-auto p-4 sm:p-6 space-y-4">
        <div className="text-xs text-stone-600 flex flex-wrap gap-2 items-center"><span>payOS: {config.data?.status ?? (config.isError ? 'UNAVAILABLE' : 'Đang tải')}</span><span>Máy chủ: {serverReachable ? 'Có kết nối' : 'Chưa kết nối'}</span><span>SSE: {status}</span><button type="button" onClick={forceReconnect} className="underline">Kết nối lại</button></div>
        {(!networkOnline || !serverReachable) && <p role="status" className="rounded-xl bg-amber-50 p-3 text-sm text-amber-800">Đang chờ xác nhận. Giữ nguyên đơn và mã QR chưa hết hạn khi mất mạng.</p>}
        {config.data && !config.data.ready && <p className="rounded-xl bg-amber-50 p-3 text-sm text-amber-800">Hiện chưa nhận đơn mới. Các đơn đã tạo vẫn được xác nhận, không chuyển sang QR ngân hàng khác.</p>}
        {notice && <p role="alert" className="text-sm text-amber-800">{notice}</p>}
        {activeModeTab === 'fixed' && <FixedPaymentQR staticUrl={config.data?.staticUrl} />}
        {activeModeTab === 'payment' && <>
          <form onSubmit={addSlot} className="rounded-2xl bg-stone-50 border border-stone-200 p-4 space-y-3">
            <div className="grid sm:grid-cols-2 gap-3">
              <label className="text-xs font-semibold space-y-1"><span>Tên khách (tùy chọn)</span><input value={nameInput} onChange={(e) => setNameInput(e.target.value)} maxLength={80} placeholder="Khách 1" className="block w-full px-3 py-2 rounded-xl border border-stone-300 bg-white" /></label>
              <label className="text-xs font-semibold space-y-1"><span>Số tiền · nghìn đồng</span><input type="text" inputMode="numeric" value={amountInput} onChange={(e) => setAmountInput(e.target.value)} placeholder="50 = 50.000đ" className="block w-full px-3 py-2 rounded-xl border border-stone-300 bg-white" /></label>
            </div>
            <p className="text-sm font-semibold">{preview === null ? 'Nhập số nguyên dương, chỉ chữ số (đơn vị nghìn đồng).' : `Xác nhận số tiền: ${formatVndCurrency(preview)} (${preview} VND)`}</p>
            <button type="submit" disabled={preview === null || !config.data?.ready || !networkOnline || slots.length >= MAX_PAYMENT_ORDER_SLOTS} className="inline-flex items-center gap-2 rounded-xl bg-emerald-600 text-white px-4 py-2 text-sm font-bold disabled:opacity-40"><Plus className="w-4 h-4" />Thêm khách & tạo đơn ({slots.length}/3)</button>
          </form>
          <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
            {slots.map((slot) => <OrderCard key={slot.slotId} slot={slot} isPublic={isPublic} isOpen={isOpen} now={now} creating={creatingKeys.includes(slot.idempotencyKey)} onRetry={(item) => { void postSlot(item); }} onRemove={removeSlot} />)}
          </div>
          <p className="text-xs text-stone-500">Bỏ thẻ chỉ bỏ khỏi khay, không hủy đơn. Giao dịch đến sau vẫn có trong lịch sử và thông báo.</p>
          {liveCredits.length > 0 && <section className="rounded-2xl border border-emerald-200 p-3"><h4 className="font-semibold text-sm text-emerald-800">Tiền vào trực tiếp</h4>{liveCredits.map((credit) => <p key={credit.transactionId} className="text-xs py-1">{formatVndCurrency(Number(credit.credit))} · {credit.bank} · {credit.transactionNumber}</p>)}</section>}
        </>}
        {activeModeTab === 'history' && <section className="space-y-3"><h4 className="font-bold">Tiền vào hôm nay</h4>{history.isLoading && <p>Đang tải lịch sử…</p>}{history.isError && <p role="alert">Không tải được lịch sử. Hãy kết nối lại.</p>}{history.data?.items.map((tx) => <div key={tx.id} className="rounded-xl border border-stone-200 p-3 text-sm"><p className="font-semibold text-emerald-700">{formatVndCurrency(Number(tx.credit))} · {tx.bank ?? 'ACB'}</p><p className="text-xs text-stone-500">{tx.orderCode ?? tx.semanticKey} · {tx.description}</p></div>)}</section>}
        {activeModeTab === 'wifi' && (
          <div className="p-6 sm:p-8 flex flex-col items-center justify-center text-center max-w-xl mx-auto w-full space-y-5 overflow-y-auto">
            <div className="space-y-1">
              <h3 className="text-base sm:text-lg font-bold text-stone-900 flex items-center justify-center gap-2">
                <Wifi className="w-5 h-5 text-blue-600" />
                Mã QR kết nối WiFi tự động
              </h3>
              <p className="text-xs text-stone-500 max-w-md">
                Khách dùng camera điện thoại (iPhone hoặc Android) quét mã này để tự kết nối WiFi mà không cần gõ mật khẩu thủ công.
              </p>
            </div>

            {/* QR Card */}
            {wifiQRDataURL ? (
              <div className="p-4 sm:p-5 bg-white rounded-3xl border border-stone-200 shadow-md">
                <div className="aspect-square w-56 sm:w-64 bg-white flex items-center justify-center rounded-2xl overflow-hidden">
                  <img
                    src={wifiQRDataURL}
                    alt="Mã QR kết nối WiFi"
                    className="w-full h-full object-contain"
                  />
                </div>
              </div>
            ) : (
              <div className="p-8 bg-stone-50 rounded-3xl border border-dashed border-stone-300 max-w-sm w-full space-y-2">
                <Wifi className="w-10 h-10 text-stone-400 mx-auto" />
                <p className="text-xs font-bold text-stone-700">Chưa thiết lập WiFi quán</p>
                <p className="text-[11px] text-stone-500">
                  Bấm "Sửa thông tin WiFi" bên dưới để nhập Tên WiFi và Mật khẩu.
                </p>
              </div>
            )}

            {/* WiFi Credentials Box */}
            {wifiSettings.ssid && (
              <div className="w-full max-w-sm bg-stone-50 rounded-2xl p-3.5 border border-stone-200 space-y-2 text-xs text-left">
                <div className="flex items-center justify-between">
                  <span className="text-stone-500 font-medium">Tên WiFi (SSID):</span>
                  <div className="flex items-center gap-1.5 font-bold text-stone-900">
                    <span>{wifiSettings.ssid}</span>
                    <button
                      type="button"
                      onClick={() => handleCopyWifiText(wifiSettings.ssid, 'ssid')}
                      className="p-1 text-stone-400 hover:text-stone-700 rounded transition cursor-pointer"
                      title="Sao chép tên WiFi"
                    >
                      {wifiCopiedField === 'ssid' ? (
                        <Check className="w-3.5 h-3.5 text-emerald-600" />
                      ) : (
                        <Copy className="w-3.5 h-3.5" />
                      )}
                    </button>
                  </div>
                </div>

                {wifiSettings.security !== 'nopass' && (
                  <div className="flex items-center justify-between">
                    <span className="text-stone-500 font-medium">Mật khẩu:</span>
                    <div className="flex items-center gap-1.5 font-mono font-bold text-stone-900">
                      <span>{showWifiPassword ? wifiSettings.password : '••••••••'}</span>
                      <button
                        type="button"
                        onClick={() => setShowWifiPassword((prev) => !prev)}
                        className="p-1 text-stone-400 hover:text-stone-700 rounded transition cursor-pointer"
                        title={showWifiPassword ? 'Ẩn mật khẩu' : 'Hiện mật khẩu'}
                      >
                        {showWifiPassword ? (
                          <EyeOff className="w-3.5 h-3.5" />
                        ) : (
                          <Eye className="w-3.5 h-3.5" />
                        )}
                      </button>
                      {wifiSettings.password && (
                        <button
                          type="button"
                          onClick={() => handleCopyWifiText(wifiSettings.password || '', 'password')}
                          className="p-1 text-stone-400 hover:text-stone-700 rounded transition cursor-pointer"
                          title="Sao chép mật khẩu"
                        >
                          {wifiCopiedField === 'password' ? (
                            <Check className="w-3.5 h-3.5 text-emerald-600" />
                          ) : (
                            <Copy className="w-3.5 h-3.5" />
                          )}
                        </button>
                      )}
                    </div>
                  </div>
                )}
              </div>
            )}

            {/* Edit WiFi Form / Toggle */}
            {!isEditingWifi ? (
              <button
                type="button"
                onClick={() => {
                  setWifiEditForm(wifiSettings);
                  setIsEditingWifi(true);
                }}
                className="inline-flex items-center gap-1.5 px-4 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-300 text-stone-700 hover:bg-stone-50 shadow-2xs transition cursor-pointer"
              >
                <Edit3 className="w-3.5 h-3.5 text-stone-500" />
                Sửa thông tin WiFi quán
              </button>
            ) : (
              <form
                onSubmit={handleSaveWifi}
                className="w-full max-w-sm bg-white p-4 rounded-2xl border border-stone-200 shadow-xs space-y-3 text-left text-xs animate-in fade-in zoom-in-95 duration-100"
              >
                <h4 className="font-bold text-stone-800">Cập nhật thông tin WiFi</h4>
                <div>
                  <label htmlFor="counter-wifi-ssid" className="block text-stone-600 font-medium mb-1">Tên WiFi (SSID)</label>
                  <input
                    id="counter-wifi-ssid"
                    type="text"
                    required
                    value={wifiEditForm.ssid}
                    onChange={(e) => setWifiEditForm((p) => ({ ...p, ssid: e.target.value }))}
                    placeholder="Ví dụ: ACB_Coffee"
                    className="w-full px-3 py-2 border border-stone-300 rounded-xl focus:outline-none focus:border-blue-600"
                  />
                </div>
                <div>
                  <label htmlFor="counter-wifi-password" className="block text-stone-600 font-medium mb-1">Mật khẩu WiFi</label>
                  <input
                    id="counter-wifi-password"
                    type="text"
                    value={wifiEditForm.password || ''}
                    onChange={(e) => setWifiEditForm((p) => ({ ...p, password: e.target.value }))}
                    placeholder="Để trống nếu không có mật khẩu"
                    className="w-full px-3 py-2 border border-stone-300 rounded-xl focus:outline-none focus:border-blue-600 font-mono"
                  />
                </div>
                <div className="flex items-center justify-between gap-3">
                  <div className="flex-1">
                    <label htmlFor="counter-wifi-security" className="block text-stone-600 font-medium mb-1">Chuẩn bảo mật</label>
                    <select
                      id="counter-wifi-security"
                      value={wifiEditForm.security}
                      onChange={(e) =>
                        setWifiEditForm((p) => ({
                          ...p,
                          security: e.target.value as 'WPA' | 'WEP' | 'nopass',
                        }))
                      }
                      className="w-full px-2.5 py-2 border border-stone-300 rounded-xl focus:outline-none focus:border-blue-600 bg-white"
                    >
                      <option value="WPA">WPA / WPA2 (Phổ biến)</option>
                      <option value="WEP">WEP (Cũ)</option>
                      <option value="nopass">Không có mật khẩu</option>
                    </select>
                  </div>
                  <label className="flex items-center gap-1.5 pt-5 cursor-pointer select-none text-stone-600">
                    <input
                      type="checkbox"
                      checked={Boolean(wifiEditForm.hidden)}
                      onChange={(e) => setWifiEditForm((p) => ({ ...p, hidden: e.target.checked }))}
                      className="rounded text-blue-600"
                    />
                    <span>Mạng ẩn</span>
                  </label>
                </div>
                <div className="pt-2 flex items-center justify-end gap-2">
                  <button
                    type="button"
                    onClick={() => setIsEditingWifi(false)}
                    className="px-3 py-1.5 rounded-xl border border-stone-200 text-stone-600 hover:bg-stone-50 font-medium transition cursor-pointer"
                  >
                    Hủy
                  </button>
                  <button
                    type="submit"
                    className="px-4 py-1.5 rounded-xl bg-blue-600 hover:bg-blue-700 text-white font-bold transition cursor-pointer shadow-xs"
                  >
                    Lưu & Tạo mã QR
                  </button>
                </div>
              </form>
            )}
          </div>
        )}
      </div>
    </div>
  </div>;
};
