import React, { useEffect, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { fetchPaymentConfig, fetchSePayStore, fetchTransactions } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { isPublicViewerHost } from '../../app/runtime-mode';
import { useRealtimeContext } from '../../realtime/RealtimeProvider';
import { formatVndCurrency } from '../../shared/formatters/money';
import { formatDateTimeVN } from '../../shared/formatters/datetime';
import { isTerminalPaymentOrder, loadPaymentOrderTray, parseCounterAmountVnd, type PaymentOrder } from './payment-orders';
import { PaymentQRImage } from './PaymentQRImage';
import { CounterUtilities } from './CounterUtilities';
import { COUNTER_ARCHIVE_PAGE_SIZE, useCounterPayments } from './useCounterPayments';

type ActiveQR = { kind: 'store' } | { kind: 'payos'; slotId: string };
const ACTIVE_QR_KEY = 'counter_active_qr_v1';
const statusLabels: Record<PaymentOrder['status'], string> = {
  CREATING: 'Đang tạo đơn', PENDING: 'Đang chờ thanh toán', PROCESSING: 'Đang xử lý', UNDERPAID: 'Chưa đủ số tiền',
  PAID: 'Đã thanh toán', CANCELLED: 'Đã hủy', EXPIRED: 'Đã hết hạn', FAILED: 'Không tạo được đơn',
};
const buttonClass = 'min-h-11 rounded-xl border border-stone-300 px-3 py-2 text-sm font-semibold focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-emerald-600 disabled:opacity-40';

export const CounterCheckout: React.FC = () => {
  const isPublic = isPublicViewerHost();
  const client = useQueryClient();
  const realtime = useRealtimeContext();
  const config = useQuery({ queryKey: queryKeys.paymentConfig, queryFn: fetchPaymentConfig, refetchInterval: 30_000 });
  const store = useQuery({ queryKey: queryKeys.sepayStore, queryFn: fetchSePayStore, refetchOnMount: 'always', refetchOnWindowFocus: 'always', refetchOnReconnect: 'always' });
  const recent = useQuery({ queryKey: queryKeys.transactions({ direction: 'credit', limit: 5 }), queryFn: () => fetchTransactions({ direction: 'credit', limit: 5 }) });
  const [activeQr, setActiveQr] = useState<ActiveQR>(() => {
    try {
      const saved: unknown = JSON.parse(sessionStorage.getItem(ACTIVE_QR_KEY) ?? 'null');
      if (saved && typeof saved === 'object' && 'kind' in saved && saved.kind === 'payos' && 'slotId' in saved && typeof saved.slotId === 'string') {
        const tray = loadPaymentOrderTray();
        if ([...tray.visible, ...tray.archived].some((slot) => slot.slotId === saved.slotId)) return { kind: 'payos', slotId: saved.slotId };
      }
    } catch { /* Selection persistence is optional; payment intent persistence is not. */ }
    return { kind: 'store' };
  });
  const [archivePage, setArchivePage] = useState<number | null>(null);
  const payments = useCounterPayments(isPublic, activeQr.kind === 'payos' ? activeQr.slotId : undefined, archivePage);
  const [amount, setAmount] = useState('');
  const [name, setName] = useState('');
  const [nameOpen, setNameOpen] = useState(() => typeof window !== 'undefined' && window.matchMedia('(min-width: 1024px)').matches);
  const [now, setNow] = useState(Date.now());
  const [removeConfirmation, setRemoveConfirmation] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState<string | null>(null);
  const [enlarged, setEnlarged] = useState(false);
  const [copyNotice, setCopyNotice] = useState('');
  const amountRef = useRef<HTMLInputElement>(null);
  const dialogRef = useRef<HTMLDivElement>(null);
  const enlargeButtonRef = useRef<HTMLButtonElement>(null);
  const submitLocked = useRef(false);
  const activeSlot = activeQr.kind === 'payos' ? payments.allSlots.find((slot) => slot.slotId === activeQr.slotId) : undefined;
  const order = activeSlot ? payments.orders[activeSlot.slotId] : undefined;
  const expired = !!order && Date.parse(order.expiresAt) <= now;
  const payable = !!order && !expired && !isTerminalPaymentOrder(order.status);
  const storeReady = store.data?.status === 'ACTIVE' && !!store.data.qrPayload;
  const preview = parseCounterAmountVnd(amount, config.data?.maxAmountVnd);
  const payload = activeQr.kind === 'store' ? (storeReady ? store.data!.qrPayload : '') : (payable ? order!.qrCode ?? '' : '');
  const select = (next: ActiveQR) => { setActiveQr(next); setCopyNotice(''); };
  useEffect(() => {
    try { sessionStorage.setItem(ACTIVE_QR_KEY, JSON.stringify(activeQr)); } catch { /* No receiver or payment data is cached offline. */ }
  }, [activeQr]);
  useEffect(() => {
    if (activeQr.kind === 'payos' && !payments.allSlots.some((slot) => slot.slotId === activeQr.slotId)) setActiveQr({ kind: 'store' });
  }, [activeQr, payments.allSlots]);
  useEffect(() => {
    if (window.matchMedia('(min-width: 1024px)').matches) amountRef.current?.focus({ preventScroll: true });
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, []);
  useEffect(() => {
    const recover = () => {
      void client.invalidateQueries({ queryKey: queryKeys.sepayStore });
      void client.invalidateQueries({ queryKey: queryKeys.transactions() });
    };
    if (realtime.status === 'CONNECTED') recover();
    window.addEventListener('focus', recover);
    window.addEventListener('online', recover);
    window.addEventListener('pageshow', recover);
    return () => { window.removeEventListener('focus', recover); window.removeEventListener('online', recover); window.removeEventListener('pageshow', recover); };
  }, [client, realtime.status, realtime.reconnectCount]);
  useEffect(() => {
    if (!enlarged) return;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : enlargeButtonRef.current;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    dialogRef.current?.focus();
    const keydown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') { event.preventDefault(); setEnlarged(false); }
      if (event.key !== 'Tab') return;
      const nodes = dialogRef.current?.querySelectorAll<HTMLElement>('button:not([disabled]), a[href], input:not([disabled]), [tabindex="0"]');
      if (!nodes?.length) { event.preventDefault(); return; }
      const first = nodes[0], last = nodes[nodes.length - 1];
      if (event.shiftKey && (document.activeElement === first || document.activeElement === dialogRef.current)) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && (document.activeElement === last || document.activeElement === dialogRef.current)) { event.preventDefault(); first.focus(); }
    };
    const keepFocus = (event: FocusEvent) => { if (event.target instanceof Node && !dialogRef.current?.contains(event.target)) dialogRef.current?.focus(); };
    document.addEventListener('keydown', keydown); document.addEventListener('focusin', keepFocus);
    return () => { document.body.style.overflow = previousOverflow; document.removeEventListener('keydown', keydown); document.removeEventListener('focusin', keepFocus); opener?.focus({ preventScroll: true }); };
  }, [enlarged]);
  const nextCustomer = () => {
    select({ kind: 'store' });
    if (window.matchMedia('(min-width: 1024px)').matches) amountRef.current?.focus({ preventScroll: true });
  };
  const archiveSlot = async (slotId: string) => {
    if (!await payments.removeSlot(slotId)) return;
    setActiveQr((current) => current.kind === 'payos' && current.slotId === slotId ? { kind: 'store' } : current);
    setRemoveConfirmation(null);
  };
  const selectedContent = <>
    <p className="text-sm font-bold text-emerald-800">{activeQr.kind === 'store' ? 'QR chuyển khoản cửa hàng · SePay' : `payOS · ${activeSlot?.name ?? 'Đơn thanh toán'}`}</p>
    <p className="text-2xl font-bold text-stone-900">{activeQr.kind === 'store' ? 'Khách tự nhập số tiền' : activeSlot ? formatVndCurrency(activeSlot.amountVnd) : ''}</p>
    {activeQr.kind === 'store' ? storeReady ? <>
      <PaymentQRImage payload={payload} alt="QR chuyển khoản cửa hàng SePay" />
      <p className="font-semibold">{store.data!.storeName}</p><p className="text-sm">{store.data!.bank} · {store.data!.accountName}</p><p className="font-mono break-all select-all">{store.data!.accountNumber}</p>
      <p className="text-sm text-stone-600">Quét bằng ứng dụng ngân hàng</p>
    </> : <div className="w-full rounded-xl bg-stone-50 p-3 text-left"><p className="font-semibold">QR cửa hàng chưa sẵn sàng</p><p className="text-sm text-stone-600">{store.isPending ? 'Đang tải cấu hình cửa hàng…' : 'Nhập tiền phía trên để dùng QR payOS.'}</p><button type="button" className="min-h-11 text-sm underline" onClick={() => void store.refetch()}>Kiểm tra QR cửa hàng</button></div> : order?.status === 'PAID' ? <div className="w-full rounded-2xl bg-emerald-50 p-5 text-emerald-900 space-y-2">
      <h3 className="text-xl font-bold">Thanh toán thành công</h3><p>Đã nhận {formatVndCurrency(order.amountVnd)}</p><p className="text-sm">Mã đơn: {order.orderCode}</p>{order.paidAt && <p className="text-sm">{formatDateTimeVN(order.paidAt)}</p>}<p className="text-xs">Giữ biên nhận. Không thanh toán lại đơn này.</p>
    </div> : order && (expired || isTerminalPaymentOrder(order.status)) ? <div className="min-h-64 flex items-center justify-center rounded-xl bg-amber-50 p-5 text-sm text-amber-900">{expired && !isTerminalPaymentOrder(order.status) ? 'Đã hết thời gian hiển thị QR. Đang chờ xác nhận trạng thái từ máy chủ.' : `${statusLabels[order.status]}. QR này không còn dùng để thanh toán.`}</div> : payable ? <>
      {payload ? <PaymentQRImage payload={payload} alt={`QR thanh toán ${activeSlot!.name}`} /> : <p className="min-h-64 flex items-center text-sm">Đang chờ mã QR từ payOS.</p>}
      <p className="font-semibold">KienlongBank · {order!.accountName}</p><p className="font-mono break-all select-all">{order!.accountNumber}</p><p className="text-sm">Mã đơn: {order!.orderCode}</p>
      <p className="text-xs text-stone-600">Hạn QR: {formatDateTimeVN(order!.expiresAt)}</p>
      {order!.checkoutUrl && <a href={order!.checkoutUrl} target="_blank" rel="noreferrer" className={`${buttonClass} text-emerald-800`}>Thanh toán trên payOS</a>}
    </> : <div className="min-h-64 flex items-center justify-center rounded-xl bg-stone-50 p-5 font-semibold">{activeSlot && payments.creatingKeys.includes(activeSlot.idempotencyKey) ? 'Đang tạo QR payOS' : 'Đang chờ xác nhận QR payOS'}</div>}
  </>;
  return <section id="counter-checkout" aria-label="Thu ngân" className="scroll-mt-4 space-y-4 [&_button]:cursor-pointer [&_button]:focus-visible:outline-2 [&_button]:focus-visible:outline-offset-2 [&_button]:focus-visible:outline-emerald-600 [&_input]:focus-visible:outline-2 [&_input]:focus-visible:outline-emerald-600">
    <div className="flex flex-wrap items-center justify-between gap-2"><h2 className="text-xl font-bold">Thu ngân</h2><p className="text-xs text-stone-600">Nhập tiền · Enter · khách tiếp theo</p></div>
    {(!realtime.networkOnline || !realtime.serverReachable || realtime.status !== 'CONNECTED') && <p role="status" className="rounded-xl bg-amber-50 px-3 py-2 text-sm text-amber-900">Đang mất cập nhật — kiểm tra nhận tiền trước khi giao hàng</p>}
    <div className="grid min-w-0 items-start gap-4 md:grid-cols-[320px_minmax(0,1fr)] lg:grid-cols-[380px_minmax(0,1fr)]">
      <form className="min-w-0 rounded-2xl border border-stone-200 bg-white p-3 sm:p-4 space-y-2 md:col-start-2" onSubmit={async (event) => {
        event.preventDefault(); if (submitLocked.current) return;
        submitLocked.current = true;
        try {
          const slot = await payments.addSlot(amount, name);
          if (slot) {
            select({ kind: 'payos', slotId: slot.slotId });
            setAmount((current) => current === amount ? '' : current);
            setName((current) => current === name ? '' : current);
            amountRef.current?.focus({ preventScroll: true });
          }
        } finally { submitLocked.current = false; }
      }}>
        <label htmlFor="counter-amount" className="block text-sm font-semibold">Số tiền · nghìn đồng</label>
        <div className="flex gap-2"><input ref={amountRef} id="counter-amount" type="text" inputMode="numeric" autoComplete="off" value={amount} onChange={(event) => setAmount(event.target.value)} placeholder="50 = 50.000đ" aria-describedby="counter-amount-preview" className="min-h-12 min-w-0 w-full rounded-xl border border-stone-300 px-3 text-2xl font-bold" /><button type="submit" disabled={preview === null || !config.data?.ready || !realtime.networkOnline} className={`${buttonClass} shrink-0 bg-emerald-700 text-white border-emerald-700`}>Tạo QR payOS</button></div>
        <div className="flex flex-wrap items-center justify-between gap-2"><p id="counter-amount-preview" className="text-lg font-bold text-emerald-800">{preview === null ? '50 → 50.000đ' : formatVndCurrency(preview)}</p><span className="text-xs text-stone-500">Enter để tạo · không giới hạn lượt khách</span></div>
        <details className="text-sm text-stone-600" open={nameOpen || Boolean(name)} onToggle={(e) => setNameOpen(e.currentTarget.open)}><summary className="min-h-11 cursor-pointer py-3">Thêm tên khách</summary><label className="block">Tên khách (tùy chọn)<input value={name} onChange={(event) => setName(event.target.value)} maxLength={80} className="mt-1 block min-h-11 w-full rounded-xl border border-stone-300 px-3 text-stone-900" /></label></details>
        {config.data && !config.data.ready && <p className="text-sm text-amber-900">payOS chưa sẵn sàng nhận đơn mới. QR cửa hàng và các đơn hiện có vẫn được giữ nguyên.</p>}
        {config.isError && <p role="alert" className="text-sm text-amber-900">Chưa tải được cấu hình payOS. Không tạo đơn mới khi chưa xác nhận dịch vụ.</p>}
        {payments.notice && <p role="alert" className="text-sm text-amber-900">{payments.notice}</p>}
      </form>
      <div className="min-w-0 space-y-2 md:col-start-1 md:row-start-1 md:row-span-3">
        <div className="flex items-center gap-2" aria-label="Chọn QR"><button type="button" aria-pressed={activeQr.kind === 'store'} className={`${buttonClass} ${activeQr.kind === 'store' ? 'bg-emerald-50 border-emerald-600 text-emerald-900' : ''}`} onClick={() => select({ kind: 'store' })}>QR cửa hàng · SePay</button>{activeQr.kind === 'payos' && <span className="min-w-0 truncate text-sm font-semibold">payOS · {activeSlot?.name}</span>}</div>
        <div data-testid="counter-active-qr" data-qr-kind={activeQr.kind} data-slot-id={activeQr.kind === 'payos' ? activeQr.slotId : undefined} className="min-w-0 rounded-2xl border border-stone-200 bg-white p-3 sm:p-4 text-center flex flex-col items-center gap-2"><div aria-hidden={enlarged} className={`flex w-full flex-col items-center gap-2 ${enlarged ? 'invisible' : ''}`}>{selectedContent}</div>
          {(payload || order) && <div className="flex flex-wrap justify-center gap-2"><button ref={enlargeButtonRef} type="button" className={buttonClass} onClick={() => setEnlarged(true)}>Phóng to QR</button>{payload && <button type="button" className={buttonClass} onClick={() => { const value = activeQr.kind === 'store' ? store.data!.accountNumber : `${order!.accountNumber ?? ''}\n${order!.amountVnd} VND\nDH${order!.orderCode}`; void navigator.clipboard.writeText(value).then(() => setCopyNotice('Đã sao chép thông tin nhận tiền'), () => setCopyNotice('Không thể sao chép. Hãy chọn thông tin trên QR.')); }}>Sao chép thông tin</button>}</div>}
          {copyNotice && <p role="status" className="text-sm">{copyNotice}</p>}
          {activeSlot && !activeSlot.orderId && <button type="button" className={buttonClass} disabled={payments.creatingKeys.includes(activeSlot.idempotencyKey)} onClick={() => payments.retrySlot(activeSlot)}>Thử lại cùng đơn</button>}
          {activeSlot && payments.slotErrors[activeSlot.slotId] && <p role="alert" className="text-sm text-amber-900">{payments.slotErrors[activeSlot.slotId]}</p>}
        </div>
        <button type="button" className={`${buttonClass} w-full`} onClick={nextCustomer}>Khách tiếp theo</button>
      </div>
      <div className="min-w-0 space-y-2 md:col-start-2 md:row-start-2">
        <div className="flex items-center justify-between gap-2"><h3 className="text-sm font-semibold">Khách gần đây</h3><button type="button" aria-expanded={archivePage !== null} aria-controls="counter-archive" className="min-h-11 text-sm underline" onClick={() => setArchivePage((current) => current === null ? 0 : null)}>Đơn trước ({payments.archived.length})</button></div>
        <div className="grid min-w-0 gap-2 sm:grid-cols-3" aria-label="Khay khách">
          {payments.slots.map((slot) => {
            const snapshot = payments.orders[slot.slotId];
            const creating = payments.creatingKeys.includes(slot.idempotencyKey);
            return <article aria-label={slot.name} key={slot.slotId} className={`min-w-0 rounded-xl border bg-white p-2 ${snapshot?.status === 'PAID' ? 'border-emerald-500' : activeQr.kind === 'payos' && activeQr.slotId === slot.slotId ? 'border-emerald-700' : 'border-stone-200'}`}>
              <button type="button" className="min-h-11 w-full text-left" aria-label={`Hiện QR ${slot.name}`} aria-pressed={activeQr.kind === 'payos' && activeQr.slotId === slot.slotId} onClick={() => select({ kind: 'payos', slotId: slot.slotId })}><span className="block truncate text-xs text-stone-600">{slot.name}</span><strong className="text-lg text-emerald-800">{formatVndCurrency(slot.amountVnd)}</strong></button>
              <p role="status" aria-live="polite" className="text-xs font-semibold">{snapshot ? statusLabels[snapshot.status] : creating ? 'Đang tạo đơn' : 'Đang chờ xác nhận'}</p>
              <details className="text-xs"><summary className="min-h-11 cursor-pointer py-3">Thao tác đơn</summary>
                {snapshot && <p className="break-all">Mã đơn: {snapshot.orderCode}</p>}
                <button type="button" className="min-h-11 underline" onClick={() => select({ kind: 'payos', slotId: slot.slotId })}>Hiện QR</button>
                {!slot.orderId && <button type="button" className={`${buttonClass} w-full`} disabled={creating} onClick={() => payments.retrySlot(slot)}>Thử lại cùng đơn</button>}
                {slot.orderId && <button type="button" className="min-h-11 text-sm underline" onClick={payments.refreshOrders}>Làm mới trạng thái</button>}
                {payments.slotErrors[slot.slotId] && <p role="alert" className="text-xs text-amber-900">{payments.slotErrors[slot.slotId]}</p>}
                {snapshot?.status === 'PAID' ? <button type="button" className="min-h-11 text-sm underline" onClick={() => void archiveSlot(slot.slotId)}>Xong, bỏ khỏi khay</button> : removeConfirmation === slot.slotId ? <div className="rounded-xl bg-amber-50 p-2 text-sm"><p>Bỏ khỏi khay không hủy đơn</p><button type="button" className="min-h-11 underline mr-2" onClick={() => void archiveSlot(slot.slotId)}>Xác nhận bỏ khỏi khay</button><button type="button" className="min-h-11 underline" onClick={() => setRemoveConfirmation(null)}>Giữ lại</button></div> : <button type="button" className="min-h-11 text-sm underline" onClick={() => setRemoveConfirmation(slot.slotId)}>Bỏ khỏi khay</button>}
                {!isPublic && snapshot && !isTerminalPaymentOrder(snapshot.status) && <button type="button" disabled={cancelling === slot.slotId} className="min-h-11 text-sm font-semibold text-rose-800" onClick={() => { setCancelling(slot.slotId); void payments.cancelSlot(slot.slotId).finally(() => setCancelling(null)); }}>{cancelling === slot.slotId ? 'Đang xác nhận hủy…' : 'Hủy đơn trên payOS'}</button>}
              </details>
            </article>;
          })}
        </div>
        {archivePage !== null && <section id="counter-archive" aria-label="Đơn trước" className="max-h-80 overflow-y-auto rounded-xl border border-stone-200 bg-stone-50 p-3 space-y-2">
          <div className="flex items-center justify-between"><h3 className="font-semibold text-sm">Đơn trước · không hủy khi lưu trữ</h3><button type="button" className="min-h-11 text-sm underline" onClick={() => setArchivePage(null)}>Đóng đơn trước</button></div>
          {payments.archivedPage.map((slot) => {
            const snapshot = payments.orders[slot.slotId];
            const creating = payments.creatingKeys.includes(slot.idempotencyKey);
            return <article key={slot.slotId} aria-label={slot.name} className="rounded-lg border border-stone-200 bg-white p-2 text-sm">
              <div className="flex flex-wrap items-center justify-between gap-2"><strong>{slot.name} · {formatVndCurrency(slot.amountVnd)}</strong><span role="status">{snapshot ? statusLabels[snapshot.status] : creating ? 'Đang tạo đơn' : slot.status ? `Lần xác nhận trước: ${statusLabels[slot.status]}` : 'Chưa xác nhận'}</span></div>
              {(snapshot?.orderCode ?? slot.orderCode) && <p className="text-xs">Mã đơn: {snapshot?.orderCode ?? slot.orderCode}</p>}
              <div className="flex flex-wrap gap-3"><button type="button" className="min-h-11 underline" onClick={() => select({ kind: 'payos', slotId: slot.slotId })}>Xem đơn / QR</button><button type="button" className="min-h-11 underline" onClick={() => payments.restoreSlot(slot.slotId)}>Đưa lại khay</button>{!slot.orderId && <button type="button" className="min-h-11 underline" disabled={creating} onClick={() => payments.retrySlot(slot)}>Thử lại cùng đơn</button>}{slot.orderId && <button type="button" className="min-h-11 underline" onClick={payments.refreshOrders}>Làm mới trạng thái</button>}</div>
              {payments.slotErrors[slot.slotId] && <p role="alert" className="text-xs text-amber-900">{payments.slotErrors[slot.slotId]}</p>}
            </article>;
          })}
          {!payments.archived.length && <p className="text-sm text-stone-600">Các đơn cũ sẽ tự lưu ở đây khi nhận khách tiếp theo.</p>}
          <div className="flex items-center justify-between gap-2"><button type="button" disabled={archivePage === 0} className={buttonClass} onClick={() => setArchivePage(Math.max(0, archivePage - 1))}>Mới hơn</button><span className="text-xs">Trang {archivePage + 1}</span><button type="button" disabled={(archivePage + 1) * COUNTER_ARCHIVE_PAGE_SIZE >= payments.archived.length} className={buttonClass} onClick={() => setArchivePage(archivePage + 1)}>Cũ hơn</button></div>
        </section>}
        {!payments.slots.length && <p className="text-sm text-stone-600">Nhập tiền và Enter để tạo mã riêng. SePay không xác nhận đơn payOS.</p>}
      </div>
      <div className="min-w-0 md:col-start-2 md:row-start-3"><CounterUtilities staticUrl={config.data?.staticUrl} /></div>
    </div>
    <details className="text-xs text-stone-600"><summary className="min-h-11 cursor-pointer py-3">Kết nối cập nhật · {realtime.status === 'CONNECTED' ? 'Đã kết nối' : 'Đang kết nối lại'}</summary><div className="flex flex-wrap items-center gap-3"><span>SePay: {store.data?.status === 'ACTIVE' ? 'Đã bật nhận thông báo' : store.data?.status === 'OBSERVING' ? 'Đang quan sát' : 'Chưa bật'}</span>{store.data?.lastMessageAt && <span>Lần nhận cuối: {formatDateTimeVN(store.data.lastMessageAt)}</span>}<button type="button" className="min-h-11 underline" onClick={realtime.forceReconnect}>Kết nối lại</button></div></details>
    <section aria-label="Giao dịch vừa nhận" className="rounded-2xl border border-stone-200 bg-white p-4 space-y-2"><h3 className="font-bold">Giao dịch vừa nhận</h3>{recent.data?.items.slice(0, 5).map((tx) => <p key={tx.id} className="text-sm py-1"><strong className="text-emerald-800">+{formatVndCurrency(Number(tx.credit))}</strong> · {tx.provider === 'SEPAY' ? 'SePay · QR cửa hàng' : tx.provider === 'PAYOS' ? `payOS · ${tx.orderCode ?? ''}` : tx.bank ?? 'ACB'} · {formatDateTimeVN(tx.transactionDate ?? tx.firstSeenAt)}</p>)}{!recent.data?.items.length && <p className="text-sm text-stone-600">{recent.isPending ? 'Đang tải giao dịch…' : recent.isError ? 'Chưa tải được giao dịch. Kiểm tra nhận tiền trước khi giao hàng.' : 'Chưa có giao dịch tiền vào.'}</p>}</section>
    {enlarged && <div className="fixed inset-0 z-50 bg-stone-950/70 flex items-center justify-center p-4"><div ref={dialogRef} tabIndex={-1} role="dialog" aria-modal="true" aria-label="Phóng to QR" className="w-full max-w-lg max-h-[90dvh] overflow-y-auto rounded-3xl bg-white p-5 flex flex-col items-center gap-3 text-center [&_img]:w-[min(75vw,380px)]">{selectedContent}<button type="button" className={`${buttonClass} w-full`} onClick={() => setEnlarged(false)}>Đóng QR phóng to</button></div></div>}
  </section>;
};
