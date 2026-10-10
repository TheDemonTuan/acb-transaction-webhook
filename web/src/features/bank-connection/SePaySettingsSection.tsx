import React, { useEffect, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../../api';
import { fetchSePayAdminConfig, fetchSePayReviews, fetchSePayTelegramStatus, registerSePayTelegram, saveSePayAdminConfig } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { formatDateTimeVN } from '../../shared/formatters/datetime';
import { PaginationControls, useCursorPagination } from '../../shared/ui/PaginationControls';
import { PaymentQRImage } from '../payment-qr/PaymentQRImage';
import { SEPAY_NOTIFICATION_TEMPLATE, sepayModeLabel, type SePayAdminConfig, type SePayAdminFields, type SePayTelegramStatus } from './sepay-admin';
import { decodeSePayQRImage, extractSePayQRReceiver, validateSePayQRPayload } from './sepay-qr';

const inputClass = 'mt-1 block min-h-11 w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-emerald-600';
const buttonClass = 'min-h-11 rounded-xl border border-stone-300 px-4 py-2 text-sm font-semibold focus:outline-none focus:ring-2 focus:ring-emerald-600 disabled:opacity-50';

const safeError = (error: unknown): string => {
  if (error instanceof ApiError) {
    const code = error.code ?? error.message;
    const messages: Record<string, string> = {
      INVALID_SEPAY_CONFIG: 'Cấu hình chưa hợp lệ. Kiểm tra thông tin người nhận, payload QR, các ID dạng số và thời điểm UTC. Chế độ kiểm tra cần đủ mọi trường; bật nhận tiền cần cả hai xác nhận.',
      SEPAY_REVISION_CONFLICT: 'Cấu hình đã được thay đổi ở phiên khác. Tải lại cấu hình rồi đối chiếu trước khi lưu; bản nhập hiện tại chưa được ghi.',
      SEPAY_RECEIVER_IMMUTABLE: 'Store này đã có lịch sử. Không thể thay ngân hàng hoặc tài khoản của cùng mã Store. Dùng mã Store mới cho người nhận mới.',
      TELEGRAM_TOKEN_REQUIRED: 'Chưa có token bot nhận. Nhập token do BotFather cấp, lưu rồi thử lại.',
      TELEGRAM_BOT_MISMATCH: 'Token không thuộc ID bot nhận đã cấu hình. Đối chiếu ID với BotFather/getMe, không dùng ID bot SePay làm ID bot nhận.',
      TELEGRAM_FOREIGN_WEBHOOK: 'Bot đang dùng webhook khác. Hệ thống không ghi đè. Dùng bot nhận riêng hoặc nhờ Owner xử lý webhook cũ.',
      TELEGRAM_REQUEST_FAILED: 'Không kết nối hoặc xác minh được Telegram. Kiểm tra token, mạng máy chủ và thử lại; chưa có xác nhận kết nối thành công.',
      TELEGRAM_CONFIG_REQUIRED: 'Lưu đầy đủ cấu hình và token trước khi đăng ký hoặc kiểm tra Telegram.',
      SEPAY_UNAVAILABLE: 'SePay tạm chưa sẵn sàng. Kiểm tra trạng thái máy chủ và thử lại. payOS vẫn độc lập.',
    };
    if (messages[code]) return messages[code];
    if (error.status === 403) return 'Chỉ Owner được cấu hình SePay. Kiểm tra tài khoản và quyền truy cập.';
    if (error.status === 409) return 'Có xung đột cấu hình. Tải lại và đối chiếu trước khi lưu.';
  }
  return 'Không thể hoàn tất thao tác SePay. Kiểm tra kết nối và thử lại. Không có token nào được hiển thị trong thông báo lỗi.';
};

const OwnerSePayForm: React.FC<{ saved: SePayAdminConfig; onSaved: (value: SePayAdminConfig) => void; onReload: () => Promise<SePayAdminConfig | undefined> }> = ({ saved, onSaved, onReload }) => {
  const [fields, setFields] = useState(saved.config);
  const [revision, setRevision] = useState(saved.revision);
  const [tokenEntered, setTokenEntered] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [busy, setBusy] = useState('');
  const [notice, setNotice] = useState<{ error: boolean; text: string } | null>(null);
  const [diagnostics, setDiagnostics] = useState<SePayTelegramStatus | null>(null);
  const [conflict, setConflict] = useState(false);
  const token = useRef<HTMLInputElement>(null);
  const uploading = useRef(false);
  const notificationAccountInvalid = fields.notificationAccountNumber !== '' && !/^[A-Za-z0-9]{1,200}$/.test(fields.notificationAccountNumber);
  useEffect(() => { if (!dirty) { setFields(saved.config); setRevision(saved.revision); } }, [saved.config, saved.revision, dirty]);
  const change = <K extends keyof SePayAdminFields>(key: K, value: SePayAdminFields[K]) => {
    setFields(current => ({ ...current, [key]: value, ...(['qrPayload', 'bankCode', 'bankName', 'accountNumber', 'accountName', 'storeKey'].includes(key) ? { receiverVerified: false } : {}), ...(['qrPayload', 'bankCode', 'bankName', 'accountNumber', 'notificationAccountNumber', 'accountName', 'botId', 'chatId', 'senderBotId', 'topicId', 'storeKey'].includes(key) ? { sourceSeparated: false } : {}) }));
    setDirty(true); setNotice(null); setDiagnostics(null);
  };
  const save = async (mode: SePayAdminFields['mode']) => {
    if (busy || uploading.current || notificationAccountInvalid) return;
    setBusy('save'); setNotice(null); setConflict(false);
    try {
      if (fields.qrPayload) validateSePayQRPayload(fields.qrPayload);
      const value = await saveSePayAdminConfig({ revision, config: { ...fields, mode }, botToken: token.current?.value ?? '' });
      if (token.current) token.current.value = '';
      setFields(value.config); setRevision(value.revision); setTokenEntered(false); setDirty(false); setDiagnostics(null); onSaved(value);
      setNotice({ error: false, text: mode === 'active' ? 'Đã bật nhận giao dịch SePay mới từ mốc kích hoạt. payOS vẫn xác nhận đơn riêng; hãy đối chiếu giao dịch thử thật trước khi giao hàng.' : mode === 'observe' ? 'Đã lưu chế độ kiểm tra. Thông báo chỉ được lưu để đối chiếu, chưa ghi tiền. Đăng ký webhook rồi thử bằng chính bot SePay.' : 'Đã lưu bản nháp. SePay chưa ghi giao dịch mới; payOS không bị thay đổi.' });
    } catch (error) {
      setConflict(error instanceof ApiError && error.status === 409);
      setNotice({ error: true, text: error instanceof ApiError ? safeError(error) : 'Không thể lưu. Kiểm tra payload QR và kết nối rồi thử lại; bản nhập được giữ nguyên.' });
    } finally { setBusy(''); }
  };
  const telegram = async (register: boolean) => {
    if (busy || dirty) return;
    setBusy(register ? 'register' : 'check'); setNotice(null);
    try {
      const result = await (register ? registerSePayTelegram() : fetchSePayTelegramStatus());
      setDiagnostics(result);
      setNotice({ error: !result.botVerified || Boolean(result.lastErrorMessage), text: result.botVerified ? register ? 'Telegram đã nhận yêu cầu đăng ký. Kiểm tra URL, hàng đợi và thử thông báo thật từ bot SePay; đây chưa phải xác nhận ngân hàng.' : 'Đã kiểm tra Telegram. Xác minh bot thành công không chứng minh SePay đang gửi tin hoặc ngân hàng đang kết nối.' : 'Chưa xác minh được bot nhận. Không kích hoạt trước khi đối chiếu cấu hình.' });
    } catch (error) { setDiagnostics(null); setNotice({ error: true, text: safeError(error) }); }
    finally { setBusy(''); }
  };
  const upload = async (file?: File) => {
    if (!file || busy || uploading.current) return;
    uploading.current = true; setBusy('decode'); setNotice(null);
    try {
      const decoded = await decodeSePayQRImage(file);
      setFields(current => ({ ...current, qrPayload: decoded.payload, ...(decoded.receiver ? { accountNumber: decoded.receiver.accountNumber, ...(decoded.receiver.bankCode ? { bankCode: decoded.receiver.bankCode } : {}) } : {}), receiverVerified: false, sourceSeparated: false }));
      setDirty(true); setDiagnostics(null);
      setNotice({ error: false, text: decoded.receiver ? 'Đã giải mã QR trên thiết bị. Đối chiếu người nhận bằng ứng dụng ngân hàng; tên ngân hàng trong mẫu Telegram phải khớp chính xác.' : 'Đã đọc payload QR, nhưng không thể trích xuất người nhận theo định dạng VietQR đã biết. Không tự điền hoặc đoán tài khoản: hãy đối chiếu và nhập thông tin Store.' });
    } catch (error) { setNotice({ error: true, text: error instanceof Error ? error.message : 'Không đọc được QR. Hãy chọn lại ảnh gốc.' }); }
    finally { uploading.current = false; setBusy(''); }
  };
  const receiver = extractSePayQRReceiver(fields.qrPayload);
  const receiverMismatch = Boolean(receiver && (fields.accountNumber !== receiver.accountNumber || (receiver.bankCode && fields.bankCode !== receiver.bankCode)));
  const requiredLabels: Partial<Record<keyof SePayAdminFields, string>> = { storeKey: 'mã Store', storeName: 'tên cửa hàng', bankCode: 'mã ngân hàng', bankName: 'tên ngân hàng trong Telegram', accountNumber: 'tài khoản/VA', accountName: 'tên người nhận', qrPayload: 'ảnh/payload QR', botId: 'ID bot nhận', chatId: 'ID nhóm', senderBotId: 'ID bot SePay', topicId: 'ID topic (0 nếu không dùng)' };
  const missing = Object.entries(requiredLabels).filter(([key]) => !String(fields[key as keyof SePayAdminFields] ?? '').trim()).map(([, label]) => label);
  if (!saved.hasBotToken && !tokenEntered) missing.push('token bot nhận');
  const canActivate = !missing.length && fields.receiverVerified && fields.sourceSeparated && !receiverMismatch && !notificationAccountInvalid;
  const textField = (key: 'storeKey' | 'storeName' | 'bankCode' | 'bankName' | 'accountNumber' | 'notificationAccountNumber' | 'accountName' | 'botId' | 'chatId' | 'senderBotId' | 'topicId' | 'activationAt', label: string, hint?: string) => <label className="block text-sm font-semibold" key={key}>{label}<input name={`sepay-${key}`} className={inputClass} value={fields[key]} onChange={event => change(key, event.target.value)} autoComplete="off" spellCheck={false} inputMode={['botId', 'chatId', 'senderBotId', 'topicId'].includes(key) ? 'text' : undefined} />{hint && <span className="mt-1 block text-xs font-normal text-stone-500">{hint}</span>}</label>;
  return <form data-testid="sepay-admin-form" autoComplete="off" onSubmit={event => { event.preventDefault(); if (canActivate) void save('active'); }} className="space-y-5">
    <fieldset disabled={Boolean(busy)} className="space-y-5 disabled:opacity-60">
      <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_280px]">
        <div className="space-y-4">
          <h4 className="font-bold">1 · QR và người nhận của Store</h4>
          <label className="block rounded-xl border border-dashed border-emerald-400 bg-emerald-50 p-4 text-sm font-semibold">Chọn ảnh QR SePay Store<input data-testid="sepay-qr-upload" type="file" accept="image/png,image/jpeg,image/webp" className="mt-2 block w-full text-xs file:mr-3 file:min-h-11 file:rounded-lg file:border-0 file:bg-emerald-700 file:px-3 file:text-white" onChange={event => { const file = event.currentTarget.files?.[0]; event.currentTarget.value = ''; void upload(file); }} /><span className="mt-2 block text-xs font-normal">PNG/JPEG/WebP · tối đa 8 MB, 4096 px mỗi chiều. Giải mã offline; không tải ảnh lên máy chủ hoặc bên thứ ba.</span></label>
          <div className="grid gap-4 sm:grid-cols-2">{textField('storeKey', 'Mã Store', 'Chữ thường, số, _ hoặc -. Đổi người nhận đã có lịch sử phải dùng mã mới.')}{textField('storeName', 'Tên cửa hàng')}{textField('bankCode', 'Mã ngân hàng', 'Mã chuẩn từ BIN đã biết; không suy ra từ logo.')}{textField('bankName', 'Tên ngân hàng trong Telegram', 'Nhập chính xác giá trị bank= trong thông báo thật.')}{textField('accountNumber', 'Tài khoản / VA trong QR', 'Giữ đúng tài khoản/VA đã giải mã từ QR; tài khoản trong thông báo Telegram không thay đổi người nhận hoặc payload QR.')}{textField('accountName', 'Tên người nhận')}</div>
          <details className="rounded-xl border p-3"><summary className="cursor-pointer text-sm font-semibold">Payload QR nguyên bản / nhập trực tiếp</summary><label className="mt-3 block text-sm">Nội dung QR<textarea name="sepay-qrPayload" rows={3} className={`${inputClass} break-all font-mono text-xs`} value={fields.qrPayload} onChange={event => change('qrPayload', event.target.value)} /></label><p className="mt-2 text-xs text-stone-500">Giữ nguyên payload đã duyệt, không thay bằng URL /pay. Chỉ payload được lưu, không lưu ảnh.</p></details>
        </div>
        <aside className="min-w-0 rounded-2xl border border-stone-200 bg-stone-50 p-4 text-center">
          <h4 className="mb-3 text-sm font-bold">QR chuyển khoản cửa hàng · SePay</h4>
          {fields.qrPayload ? <PaymentQRImage payload={fields.qrPayload} alt="Xem trước QR SePay Store đã giải mã" /> : <p className="py-8 text-sm text-stone-500">Chọn ảnh QR gốc của Store để xem người nhận.</p>}
          {receiver && <dl data-testid="sepay-decoded-receiver" className="mt-3 break-all text-left text-sm"><dt className="font-semibold">Thông tin đọc từ QR</dt><dd>BIN: {receiver.bin}{receiver.bankCode ? ` · ${receiver.bankCode}` : ' · chưa có ánh xạ ngân hàng'}</dd><dd>Tài khoản: <strong>{receiver.accountNumber}</strong></dd></dl>}
          {receiverMismatch && <p role="alert" className="mt-2 text-sm text-rose-700">Ngân hàng hoặc tài khoản nhập không khớp QR. Đối chiếu trước khi bật.</p>}
          <p className="mt-3 text-xs text-stone-500">Quét bằng ứng dụng ngân hàng để kiểm tra tên, tài khoản/VA và nội dung cố định. Không tạo chuyển khoản từ trang này.</p>
        </aside>
      </div>
      <div className="border-t pt-5">
        <h4 className="mb-3 font-bold">2 · Bot nhận Telegram riêng</h4>
        <div className="grid gap-4 sm:grid-cols-2">
          {textField('notificationAccountNumber', 'Tài khoản trong thông báo Telegram', 'Chép số sau account= trong tin SePay. Để trống nếu giống tài khoản/VA trong QR.')}
          {textField('botId', 'ID bot nhận', 'ID số của bot do Owner tạo, không phải bot SePay.')}
          {textField('senderBotId', 'ID bot gửi SePay', 'Đối chiếu ID số với thành viên bot SePay thật; không dùng tên/username.')}
          {textField('chatId', 'ID nhóm Telegram', 'ID nhóm là số âm, ví dụ -100…; giữ nguyên mọi chữ số.')}
          {textField('topicId', 'ID topic', 'Nhập 0 nếu nhóm không dùng topic.')}
        </div>
        {notificationAccountInvalid && <p role="alert" className="mt-2 text-sm text-rose-700">Tài khoản trong thông báo Telegram chỉ được chứa 1–200 chữ cái Latin hoặc chữ số, không có khoảng trắng; để trống nếu giống tài khoản/VA trong QR.</p>}
        <p className="mt-2 text-xs text-stone-500">Nếu account= là tài khoản chính thay vì VA trong QR, Owner phải cấu hình SePay lọc đúng Store/VA và loại trừ giao dịch payOS trước khi bật. Không suy ra Store từ nội dung chuyển khoản.</p>
        <label className="mt-4 block text-sm font-semibold">Token bot nhận <span className="font-normal text-stone-500">{saved.hasBotToken ? '· đã lưu, để trống để giữ' : '· chưa lưu'}</span>
          <input ref={token} name="sepay-botToken" type="password" autoComplete="new-password" spellCheck={false} className={`${inputClass} font-mono`} onChange={event => { setTokenEntered(Boolean(event.target.value)); setDirty(true); setNotice(null); setDiagnostics(null); }} />
        </label>
        <p className="mt-2 text-xs text-stone-500">Token chỉ ghi, không đọc lại; không lưu vào localStorage, sessionStorage hoặc bộ đệm truy vấn. Webhook secret do máy chủ tự tạo và giữ kín.</p>
      </div>
      <SePaySetupHelp />
      <div className="space-y-3 border-t pt-5"><h4 className="font-bold">3 · Đối chiếu trước khi bật nhận giao dịch</h4>
        <label className="flex min-h-11 items-start gap-3 text-sm"><input type="checkbox" className="mt-1 h-5 w-5 shrink-0" name="sepay-receiverVerified" checked={fields.receiverVerified} onChange={event => change('receiverVerified', event.target.checked)} />Tôi đã quét QR gốc bằng ứng dụng ngân hàng và xác nhận đúng Store, ngân hàng, tài khoản/VA, tên người nhận và nội dung cố định.</label>
        <label className="flex min-h-11 items-start gap-3 text-sm"><input type="checkbox" className="mt-1 h-5 w-5 shrink-0" name="sepay-sourceSeparated" checked={fields.sourceSeparated} onChange={event => change('sourceSeparated', event.target.checked)} />Tôi đã thử thông báo thật từ bot SePay, đối chiếu amount/account/bank/time/reference và chứng minh SePay đã lọc đúng Store/VA, nhóm này chỉ nhận tiền Store, không lẫn giao dịch payOS. Nếu thông báo dùng tài khoản chính, tôi đã xác minh bộ lọc Store/VA trước khi bật. Không suy ra Store từ nội dung chuyển khoản hoặc ghép hai nguồn theo số tiền/thời gian.</label>
        <details className="rounded-xl border p-3">
          <summary className="min-h-6 cursor-pointer text-sm font-semibold">Nâng cao · mốc bắt đầu ghi giao dịch</summary>
          <p className="my-2 text-xs text-stone-500">Thông thường không cần chỉnh: máy chủ đặt giờ hiện tại khi bật nhận giao dịch mới.</p>
          {textField('activationAt', 'Mốc bắt đầu ghi giao dịch (UTC)', 'RFC3339, ví dụ 2026-10-10T09:00:00Z. Để trống khi bật lần đầu để máy chủ đặt giờ hiện tại. Thông báo cũ không tự phát lại thành tiền mới.')}
        </details>
      </div>
      {missing.length > 0 && <p data-testid="sepay-missing-fields" className="rounded-xl bg-amber-50 p-3 text-sm text-amber-900">Chưa thiết lập đủ · Còn thiếu: {missing.join(', ')}.</p>}
      {!missing.length && (!fields.receiverVerified || !fields.sourceSeparated) && <p className="text-sm text-amber-900">Còn thiếu xác nhận đối chiếu QR/người nhận và nguồn thông báo riêng cho Store.</p>}
      <div className="flex flex-wrap gap-2">
        <button type="submit" disabled={!canActivate} className={`${buttonClass} border-emerald-700 bg-emerald-700 text-white`}>{busy === 'save' ? 'Đang lưu…' : saved.config.mode === 'active' ? 'Lưu & tiếp tục nhận SePay' : 'Lưu & bật SePay'}</button>
        <button type="button" className={buttonClass} onClick={() => void save('disabled')}>{saved.config.mode === 'disabled' ? 'Lưu bản nháp' : 'Lưu bản nháp · dừng SePay'}</button>
        <button type="button" className={`${buttonClass} bg-amber-50`} onClick={() => void save('observe')}>Lưu chế độ kiểm tra</button>
      </div>
    </fieldset>
    <div className="flex flex-wrap gap-2"><button type="button" disabled={Boolean(busy) || dirty || !saved.hasBotToken} className={buttonClass} onClick={() => void telegram(true)}>{busy === 'register' ? 'Đang đăng ký…' : 'Đăng ký webhook Telegram'}</button><button type="button" disabled={Boolean(busy) || dirty || !saved.hasBotToken} className={buttonClass} onClick={() => void telegram(false)}>{busy === 'check' ? 'Đang kiểm tra…' : 'Kiểm tra kết nối Telegram'}</button></div>
    {dirty && <p className="text-xs text-amber-800">Có thay đổi chưa lưu. Lưu cấu hình trước khi đăng ký hoặc kiểm tra kết nối.</p>}
    {busy === 'decode' && <p role="status" className="text-sm">Đang giải mã ảnh trên thiết bị…</p>}
    {notice && <p role={notice.error ? 'alert' : 'status'} className={`text-sm ${notice.error ? 'text-rose-700' : 'text-emerald-800'}`}>{notice.text}</p>}
    {conflict && <button type="button" className={buttonClass} disabled={Boolean(busy)} onClick={() => { void (async () => { setBusy('reload'); try { const next = await onReload(); if (next) { setFields(next.config); setRevision(next.revision); setTokenEntered(false); setDirty(false); setConflict(false); setDiagnostics(null); setNotice(null); if (token.current) token.current.value = ''; } else setNotice({ error: true, text: 'Không tải lại được cấu hình. Bản nhập hiện tại được giữ nguyên.' }); } finally { setBusy(''); } })(); }}>Tải lại cấu hình · bỏ bản nhập hiện tại</button>}
    {diagnostics && <dl data-testid="sepay-telegram-diagnostics" className="space-y-2 rounded-xl bg-stone-50 p-4 text-sm"><div><dt className="inline font-semibold">Bot nhận: </dt><dd className="inline">{diagnostics.botVerified ? 'Đã xác minh ID' : 'Chưa xác minh'}</dd></div><div><dt className="font-semibold">Địa chỉ webhook</dt><dd className="break-all font-mono text-xs">{diagnostics.registeredUrl || 'Chưa có webhook'}</dd></div><div><dt className="inline font-semibold">Thông báo chờ: </dt><dd className="inline">{diagnostics.pendingUpdateCount}</dd></div><div><dt className="inline font-semibold">Lỗi giao gần nhất: </dt><dd className="inline">{diagnostics.lastErrorMessage ? `${diagnostics.lastErrorAt ? `${formatDateTimeVN(diagnostics.lastErrorAt)} · ` : ''}Telegram báo lỗi giao webhook. Kiểm tra tuyến callback và kết nối máy chủ.` : 'Chưa ghi nhận'}</dd></div></dl>}
  </form>;
};

const SePaySetupHelp: React.FC = () => {
  const [copy, setCopy] = useState('');
  return <details className="rounded-xl border border-stone-200 bg-stone-50 p-4"><summary className="min-h-6 cursor-pointer text-sm font-semibold">Hướng dẫn BotFather, mẫu tin và kiểm tra nguồn</summary><ol className="mt-3 list-decimal space-y-2 pl-5 text-sm text-stone-700"><li>Tạo bot nhận riêng bằng BotFather. Bật Bot-to-Bot Communication Mode, tắt Group Privacy Mode, bỏ rồi thêm lại bot vào nhóm riêng có bot SePay. Không cần quyền admin thừa; bot nhận không trả lời nhóm.</li><li>Trong SePay chọn đúng Store, chỉ tiền vào, đúng topic (hoặc 0), tắt tự xóa tin. Đối chiếu ID số của nhóm/bot gửi từ update thật với thành viên bot SePay; không tin tên hiển thị và không tự học từ tin đầu tiên.</li><li>Chèn từng biến bằng giao diện SePay và đối chiếu thông báo thật theo mẫu dưới. Khả năng biến của gói Store phải được kiểm tra; không bật nếu thiếu reference, account hoặc thời gian đầy đủ, hoặc còn dấu {'{{…}}'} chưa render. Nếu account= là tài khoản chính, nhập riêng vào “Tài khoản trong thông báo Telegram”, giữ nguyên VA/payload QR và chứng minh bộ lọc SePay chỉ gửi đúng Store/VA, loại trừ payOS. Không nhận diện Store bằng nội dung chuyển khoản.</li><li>Lưu chế độ kiểm tra, đăng ký webhook, kiểm tra Telegram rồi thực hiện một giao dịch thử nhỏ do Owner chủ động. Chứng minh giao dịch payOS không đi vào nhóm Store. Nếu chưa tách được nguồn, giữ chế độ kiểm tra.</li><li>Chỉ bật sau khi đối chiếu và xác nhận bên dưới. Không có tin mới không có nghĩa kết nối ngân hàng bị lỗi. Telegram có giới hạn và không phải bằng chứng ngân hàng ký số; sau mất kết nối dài phải đối chiếu thủ công.</li></ol><pre className="mt-3 overflow-x-auto rounded-lg border bg-white p-3 text-xs" tabIndex={0}>{SEPAY_NOTIFICATION_TEMPLATE}</pre><button type="button" className={`${buttonClass} mt-3`} onClick={() => { void navigator.clipboard.writeText(SEPAY_NOTIFICATION_TEMPLATE).then(() => setCopy('Đã sao chép mẫu tin.'), () => setCopy('Không sao chép được. Chọn và sao chép mẫu phía trên.')); }}>Sao chép mẫu thông báo</button>{copy && <p role="status" className="mt-2 text-xs">{copy}</p>}</details>;
};

export const SePaySettingsSection: React.FC<{ isOwner: boolean; canReview: boolean; status?: { mode: string; lastMessageAt: string | null; reviewCount: number } }> = ({ isOwner, canReview, status }) => {
  const queryClient = useQueryClient();
  const config = useQuery({ queryKey: queryKeys.sepayAdminConfig, queryFn: fetchSePayAdminConfig, enabled: isOwner, retry: false, gcTime: 0 });
  const pagination = useCursorPagination(20);
  const params = { limit: pagination.pageSize, cursor: pagination.cursor };
  const reviews = useQuery({ queryKey: queryKeys.sepayReviews(params), queryFn: () => fetchSePayReviews(params), enabled: canReview, retry: false });
  const current = config.data ? { mode: config.data.config.mode, lastMessageAt: config.data.lastMessageAt, reviewCount: config.data.reviewCount } : status;
  const onSaved = (value: SePayAdminConfig) => { queryClient.setQueryData(queryKeys.sepayAdminConfig, value); void queryClient.invalidateQueries({ queryKey: queryKeys.status }); void queryClient.invalidateQueries({ queryKey: queryKeys.sepayStore }); };
  return <section id="sepay-store-settings" aria-labelledby="sepay-section-title" className="space-y-5 rounded-2xl border border-emerald-200 bg-white p-4 sm:p-6">
    <div className="flex flex-wrap items-start justify-between gap-3"><div><h3 id="sepay-section-title" className="text-lg font-bold">SePay Store · QR cửa hàng</h3><p className="mt-1 text-sm text-stone-600">QR chung và thông báo Telegram, tách biệt hoàn toàn với đơn payOS.</p></div><span className="rounded-full bg-emerald-50 px-3 py-2 text-xs font-semibold text-emerald-900">{current ? sepayModeLabel(current.mode) : 'Đang tải trạng thái'}</span></div>
    <p className="text-xs text-stone-600">Thông báo hợp lệ gần nhất: {current?.lastMessageAt ? formatDateTimeVN(current.lastMessageAt) : 'Chưa ghi nhận'} · Cần kiểm tra: {current?.reviewCount ?? '—'}. Trạng thái này không bảo đảm kết nối ngân hàng.</p>
    {isOwner ? <>
      {config.isError && <div role="alert" className="space-y-2 text-sm text-rose-700"><p>{config.data ? 'Không tải lại được cấu hình SePay. Bản nhập hiện tại được giữ nguyên; kiểm tra kết nối trước khi lưu.' : 'Không thể tải cấu hình SePay. Không hiển thị cấu hình giả hoặc ghi đè dữ liệu chưa tải.'}</p><button type="button" className={buttonClass} onClick={() => void config.refetch()}>Tải lại cấu hình SePay</button></div>}
      {config.data ? <OwnerSePayForm saved={config.data} onSaved={onSaved} onReload={async () => { const next = await config.refetch(); return next.isError ? undefined : next.data; }} /> : config.isPending ? <p role="status">Đang tải cấu hình SePay…</p> : null}
    </> : <p className="text-sm text-stone-600">Chỉ Owner được thay QR, cấu hình bot và bật nhận giao dịch. Bạn chỉ có quyền xem trạng thái.</p>}
    {canReview && <details className="border-t pt-4"><summary className="min-h-11 cursor-pointer text-sm font-bold">Thông báo SePay cần kiểm tra</summary>{reviews.isPending ? <p role="status">Đang tải…</p> : reviews.isError ? <p role="alert" className="text-sm text-rose-700">Không tải được mục cần kiểm tra. <button type="button" className="min-h-11 underline" onClick={() => void reviews.refetch()}>Thử lại</button></p> : !reviews.data?.items.length ? <p className="py-3 text-sm text-stone-500">Không có mục cần kiểm tra.</p> : <ul className="divide-y">{reviews.data.items.map(item => <li key={`${item.storeKey}:${item.messageId}:${item.receivedAt}`} className="space-y-1 py-3 text-sm"><p className="break-all font-semibold">{item.storeKey} · Tin {item.messageId}</p><p className="break-all font-mono text-xs">{item.reason}</p>{item.reason === 'ACCOUNT_MISMATCH' && <p>Tài khoản hoặc ngân hàng trong thông báo Telegram khác cấu hình. Đối chiếu chính xác account= với tài khoản thông báo đã nhập (để trống thì dùng tài khoản/VA trong QR) và bank= với tên ngân hàng trong Telegram. Tên người nhận không dùng để khớp thông báo; sửa tên không giải quyết lỗi này. Thông báo đã bị từ chối không được tự phát lại thành tiền.</p>}<p className="text-xs text-stone-500">{formatDateTimeVN(item.receivedAt)}</p></li>)}</ul>}<PaginationControls pageNumber={pagination.pageNumber} itemCount={reviews.data?.items.length ?? 0} pageSize={pagination.pageSize} hasNext={Boolean(reviews.data?.nextCursor)} hasPrev={pagination.hasPrev} isLoading={reviews.isFetching} onNext={() => pagination.handleNext(reviews.data?.nextCursor)} onPrev={pagination.handlePrev} onFirst={pagination.handleFirst} onPageSizeChange={pagination.setPageSize} /></details>}
  </section>;
};
