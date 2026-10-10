import React, { useEffect, useRef, useState } from 'react';
import { Wifi, X, Copy, Eye, EyeOff, Edit3 } from 'lucide-react';
import { PaymentQRImage } from './PaymentQRImage';
import {
  buildWifiQRString,
  loadWifiSettings,
  saveWifiSettings,
  WIFI_STORAGE_KEY,
  type WifiSettings,
} from './wifi-qr';

export interface WifiQRModalProps {
  isOpen: boolean;
  onClose: () => void;
}

export const WifiQRModal: React.FC<WifiQRModalProps> = ({ isOpen, onClose }) => {
  const [settings, setSettings] = useState<WifiSettings>(loadWifiSettings);
  const [draft, setDraft] = useState<WifiSettings>(loadWifiSettings);
  const [editing, setEditing] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
  const [notice, setNotice] = useState('');
  const dialogRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!isOpen) return;
    const current = loadWifiSettings();
    setSettings(current);
    setDraft(current);
    setEditing(false);
    setNotice('');
  }, [isOpen]);

  useEffect(() => {
    const handleStorage = (event: StorageEvent) => {
      if (event.key === WIFI_STORAGE_KEY || event.key === null) {
        const next = loadWifiSettings();
        setSettings(next);
        setDraft(next);
      }
    };
    window.addEventListener('storage', handleStorage);
    return () => window.removeEventListener('storage', handleStorage);
  }, []);

  useEffect(() => {
    if (!isOpen) return;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        onClose();
      }
    };
    document.addEventListener('keydown', handleKeyDown);
    return () => {
      document.body.style.overflow = previousOverflow;
      document.removeEventListener('keydown', handleKeyDown);
    };
  }, [isOpen, onClose]);

  if (!isOpen) return null;

  const payload = buildWifiQRString(settings.ssid, settings.password, settings.security, settings.hidden);

  const copy = (value: string, label: string) => {
    void navigator.clipboard.writeText(value).then(
      () => setNotice(`Đã sao chép ${label}`),
      () => setNotice('Không thể sao chép thông tin WiFi.')
    );
  };

  const handleSave = (event: React.FormEvent) => {
    event.preventDefault();
    const next = saveWifiSettings(draft);
    setSettings(next);
    setEditing(false);
    setNotice('Đã cập nhật thông tin WiFi thành công');
  };

  return (
    <div className="fixed inset-0 z-50 bg-stone-950/75 backdrop-blur-xs flex items-center justify-center p-3 sm:p-4 animate-in fade-in">
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="WiFi quán"
        className="w-full max-w-md max-h-[90vh] overflow-y-auto rounded-3xl bg-white p-5 sm:p-6 shadow-2xl space-y-4"
      >
        <div className="flex items-center justify-between pb-3 border-b border-stone-100">
          <div className="flex items-center gap-2.5">
            <div className="w-9 h-9 rounded-xl bg-blue-50 text-blue-600 flex items-center justify-center">
              <Wifi className="w-5 h-5" />
            </div>
            <div>
              <h3 className="font-bold text-stone-900 text-base leading-tight">WiFi quán</h3>
              <p className="text-xs text-stone-500">Kết nối mạng cho khách tại quầy</p>
            </div>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Đóng cửa sổ"
            className="w-9 h-9 flex items-center justify-center rounded-xl text-stone-400 hover:text-stone-700 hover:bg-stone-100 transition cursor-pointer"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        <section aria-label="WiFi quán" className="space-y-4">
          <p className="text-xs text-stone-600 text-center">
            Quét bằng camera để kết nối WiFi. Mã này không dùng để thanh toán.
          </p>

          <div className="flex justify-center bg-stone-50 p-4 rounded-2xl border border-stone-100">
            {payload ? (
              <PaymentQRImage payload={payload} alt="Mã QR kết nối WiFi" />
            ) : (
              <div className="py-8 text-center text-sm text-stone-500">
                Chưa thiết lập WiFi quán
              </div>
            )}
          </div>

          {settings.ssid && !editing && (
            <div className="bg-stone-50 rounded-2xl p-3.5 border border-stone-200/80 space-y-2 text-xs">
              <div className="flex items-center justify-between gap-2">
                <span className="text-stone-500">Tên WiFi (SSID):</span>
                <div className="flex items-center gap-1.5 font-bold text-stone-900">
                  <span>{settings.ssid}</span>
                  <button
                    type="button"
                    onClick={() => copy(settings.ssid, 'tên WiFi')}
                    title="Sao chép tên WiFi"
                    className="p-1 hover:bg-stone-200 rounded text-stone-600 transition"
                  >
                    <Copy className="w-3.5 h-3.5" />
                  </button>
                </div>
              </div>

              {settings.security !== 'nopass' && (
                <div className="flex items-center justify-between gap-2 pt-1 border-t border-stone-200/50">
                  <span className="text-stone-500">Mật khẩu:</span>
                  <div className="flex items-center gap-1.5 font-mono font-bold text-stone-900">
                    <span>{showPassword ? settings.password : '••••••••'}</span>
                    <button
                      type="button"
                      onClick={() => setShowPassword(!showPassword)}
                      title={showPassword ? 'Ẩn mật khẩu' : 'Hiện mật khẩu'}
                      className="p-1 hover:bg-stone-200 rounded text-stone-600 transition"
                    >
                      {showPassword ? <EyeOff className="w-3.5 h-3.5" /> : <Eye className="w-3.5 h-3.5" />}
                    </button>
                    <button
                      type="button"
                      onClick={() => copy(settings.password ?? '', 'mật khẩu WiFi')}
                      title="Sao chép mật khẩu"
                      className="p-1 hover:bg-stone-200 rounded text-stone-600 transition"
                    >
                      <Copy className="w-3.5 h-3.5" />
                    </button>
                  </div>
                </div>
              )}
            </div>
          )}

          {!editing && (
            <div className="flex gap-2">
              <button
                type="button"
                className="flex-1 min-h-11 inline-flex items-center justify-center gap-2 rounded-xl border border-stone-300 px-3 text-xs font-semibold text-stone-700 hover:bg-stone-50 transition cursor-pointer"
                onClick={() => {
                  setDraft(settings);
                  setEditing(true);
                }}
              >
                <Edit3 className="w-3.5 h-3.5" />
                Sửa thông tin WiFi
              </button>
              <button
                type="button"
                className="flex-1 min-h-11 rounded-xl bg-stone-900 px-3 text-xs font-semibold text-white hover:bg-stone-800 transition cursor-pointer"
                onClick={onClose}
              >
                Đóng
              </button>
            </div>
          )}

          {editing && (
            <form className="space-y-3 pt-1 border-t border-stone-200" onSubmit={handleSave}>
              <label className="block text-xs font-semibold text-stone-700">
                Tên WiFi (SSID)
                <input
                  required
                  value={draft.ssid}
                  onChange={(event) => setDraft({ ...draft, ssid: event.target.value })}
                  placeholder="Ví dụ: Quan_Cafe_Free"
                  className="mt-1 block min-h-10 w-full rounded-xl border border-stone-300 px-3 text-xs text-stone-900"
                />
              </label>

              <label className="block text-xs font-semibold text-stone-700">
                Bảo mật
                <select
                  value={draft.security}
                  onChange={(event) =>
                    setDraft({ ...draft, security: event.target.value as WifiSettings['security'] })
                  }
                  className="mt-1 block min-h-10 w-full rounded-xl border border-stone-300 px-3 text-xs text-stone-900 bg-white"
                >
                  <option value="WPA">WPA / WPA2</option>
                  <option value="WEP">WEP</option>
                  <option value="nopass">Không mật khẩu</option>
                </select>
              </label>

              {draft.security !== 'nopass' && (
                <label className="block text-xs font-semibold text-stone-700">
                  Mật khẩu WiFi
                  <input
                    type={showPassword ? 'text' : 'password'}
                    value={draft.password ?? ''}
                    onChange={(event) => setDraft({ ...draft, password: event.target.value })}
                    placeholder="Nhập mật khẩu WiFi"
                    className="mt-1 block min-h-10 w-full rounded-xl border border-stone-300 px-3 text-xs text-stone-900"
                  />
                </label>
              )}

              <label className="flex min-h-9 items-center gap-2 text-xs text-stone-600 cursor-pointer">
                <input
                  type="checkbox"
                  checked={!!draft.hidden}
                  onChange={(event) => setDraft({ ...draft, hidden: event.target.checked })}
                  className="rounded border-stone-300 text-emerald-600"
                />
                Mạng WiFi ẩn
              </label>

              <div className="flex gap-2 pt-2">
                <button
                  type="submit"
                  className="flex-1 min-h-10 rounded-xl bg-emerald-700 px-4 text-xs font-semibold text-white hover:bg-emerald-800 transition cursor-pointer"
                >
                  Lưu WiFi
                </button>
                <button
                  type="button"
                  className="flex-1 min-h-10 rounded-xl border border-stone-200 px-4 text-xs font-semibold text-stone-600 hover:bg-stone-50 transition cursor-pointer"
                  onClick={() => setEditing(false)}
                >
                  Hủy chỉnh sửa
                </button>
              </div>
            </form>
          )}

          {notice && (
            <p role="status" className="text-xs text-center font-medium text-emerald-700 bg-emerald-50 py-1.5 px-3 rounded-lg">
              {notice}
            </p>
          )}
        </section>
      </div>
    </div>
  );
};
