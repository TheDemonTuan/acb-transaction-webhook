import React, { useEffect, useState } from 'react';
import { PayOSLinkQR, PaymentQRImage } from './PaymentQRImage';
import { buildWifiQRString, loadWifiSettings, saveWifiSettings, WIFI_STORAGE_KEY, type WifiSettings } from './wifi-qr';

export const CounterUtilities: React.FC<{ staticUrl?: string }> = ({ staticUrl }) => {
  const [settings, setSettings] = useState(loadWifiSettings);
  const [draft, setDraft] = useState<WifiSettings>(loadWifiSettings);
  const [editing, setEditing] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
  const [notice, setNotice] = useState('');
  useEffect(() => {
    const storage = (event: StorageEvent) => {
      if (event.key === WIFI_STORAGE_KEY || event.key === null) {
        const next = loadWifiSettings(); setSettings(next); setDraft(next);
      }
    };
    window.addEventListener('storage', storage);
    return () => window.removeEventListener('storage', storage);
  }, []);
  const payload = buildWifiQRString(settings.ssid, settings.password, settings.security, settings.hidden);
  const copy = (value: string) => {
    void navigator.clipboard.writeText(value).then(() => setNotice('Đã sao chép'), () => setNotice('Không thể sao chép thông tin WiFi.'));
  };
  return <details className="rounded-2xl border border-stone-200 bg-white">
    <summary className="min-h-11 cursor-pointer px-4 py-3 font-semibold text-sm focus-visible:outline-2 focus-visible:outline-emerald-600">Tiện ích</summary>
    <div className="space-y-5 border-t border-stone-100 p-4">
      <PayOSLinkQR staticUrl={staticUrl} />
      <section aria-label="WiFi quán" className="space-y-3 border-t border-stone-200 pt-4">
        <h3 className="font-bold">WiFi quán</h3>
        <p className="text-sm text-stone-600">Quét bằng camera để kết nối WiFi. Mã này không dùng để thanh toán.</p>
        {payload ? <PaymentQRImage payload={payload} alt="Mã QR kết nối WiFi" /> : <p className="text-sm text-stone-600">Chưa thiết lập WiFi quán</p>}
        {settings.ssid && <div className="space-y-2 text-sm">
          <p>Tên WiFi (SSID): <strong>{settings.ssid}</strong></p>
          <button type="button" onClick={() => copy(settings.ssid)} className="min-h-11 underline">Sao chép tên WiFi</button>
          {settings.security !== 'nopass' && <><p>Mật khẩu: {showPassword ? settings.password : '••••••••'}</p><button type="button" className="min-h-11 underline mr-4" onClick={() => setShowPassword(!showPassword)}>{showPassword ? 'Ẩn mật khẩu' : 'Hiện mật khẩu'}</button><button type="button" className="min-h-11 underline" onClick={() => copy(settings.password ?? '')}>Sao chép mật khẩu WiFi</button></>}
        </div>}
        <button type="button" className="min-h-11 rounded-xl border border-stone-300 px-3 text-sm font-semibold" onClick={() => { setDraft(settings); setEditing(!editing); }}>Sửa thông tin WiFi</button>
        {editing && <form className="space-y-3" onSubmit={(event) => { event.preventDefault(); const next = saveWifiSettings(draft); setSettings(next); setEditing(false); }}>
          <label className="block text-sm">Tên WiFi (SSID)<input required value={draft.ssid} onChange={(event) => setDraft({ ...draft, ssid: event.target.value })} className="block min-h-11 w-full rounded-xl border border-stone-300 px-3" /></label>
          <label className="block text-sm">Bảo mật<select value={draft.security} onChange={(event) => setDraft({ ...draft, security: event.target.value as WifiSettings['security'] })} className="block min-h-11 rounded-xl border border-stone-300 px-3"><option value="WPA">WPA / WPA2</option><option value="WEP">WEP</option><option value="nopass">Không mật khẩu</option></select></label>
          {draft.security !== 'nopass' && <label className="block text-sm">Mật khẩu WiFi<input type={showPassword ? 'text' : 'password'} value={draft.password ?? ''} onChange={(event) => setDraft({ ...draft, password: event.target.value })} className="block min-h-11 w-full rounded-xl border border-stone-300 px-3" /></label>}
          <label className="flex min-h-11 items-center gap-2 text-sm"><input type="checkbox" checked={!!draft.hidden} onChange={(event) => setDraft({ ...draft, hidden: event.target.checked })} />Mạng WiFi ẩn</label>
          <button type="submit" className="min-h-11 rounded-xl bg-emerald-700 px-4 font-semibold text-white">Lưu WiFi</button>
          <button type="button" className="min-h-11 px-4" onClick={() => setEditing(false)}>Hủy chỉnh sửa</button>
        </form>}
        {notice && <p role="status" className="text-sm">{notice}</p>}
      </section>
    </div>
  </details>;
};
