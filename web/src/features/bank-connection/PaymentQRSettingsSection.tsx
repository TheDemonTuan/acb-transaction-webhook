import React, { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { QrCode, CheckCircle2, RefreshCw, Wifi, Save } from 'lucide-react';
import { fetchPaymentConfig } from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { PayOSLinkQR } from '../payment-qr/PaymentQRImage';
import { loadWifiSettings, saveWifiSettings, buildWifiQRString, generateWifiQRDataURL, type WifiSettings } from '../payment-qr/wifi-qr';

export const PaymentQRSettingsSection: React.FC = () => {
  const [wifiForm, setWifiForm] = useState<WifiSettings>(loadWifiSettings);
  const [wifiQRDataURL, setWifiQRDataURL] = useState('');
  const [wifiNotice, setWifiNotice] = useState<string | null>(null);
  const config = useQuery({ queryKey: queryKeys.paymentConfig, queryFn: fetchPaymentConfig });
  React.useEffect(() => {
    const current = loadWifiSettings();
    setWifiForm(current);
    const qrStr = buildWifiQRString(current.ssid, current.password, current.security, current.hidden);
    if (qrStr) {
      void generateWifiQRDataURL(qrStr).then(setWifiQRDataURL);
    } else {
      setWifiQRDataURL('');
    }
  }, []);

  const handleSaveWifi = (e: React.FormEvent) => {
    e.preventDefault();
    const updated = saveWifiSettings(wifiForm);
    setWifiForm(updated);
    const qrStr = buildWifiQRString(updated.ssid, updated.password, updated.security, updated.hidden);
    if (qrStr) {
      void generateWifiQRDataURL(qrStr).then(setWifiQRDataURL);
    } else {
      setWifiQRDataURL('');
    }
    setWifiNotice('Đã lưu cấu hình WiFi thành công! Cài đặt sẽ tự động đồng bộ trên Transaction Viewer.');
    setTimeout(() => setWifiNotice(null), 4000);
  };
  return <div className="bg-white rounded-2xl border border-stone-200/80 shadow-xs overflow-hidden">
    <div className="p-6 border-b border-stone-100 flex items-center justify-between gap-4">
      <div><h3 className="flex items-center gap-2 text-base font-bold text-stone-900"><QrCode className="w-5 h-5" />Liên kết nhập tiền payOS</h3><p className="text-xs text-stone-500 mt-1">KienlongBank · khách nhập số tiền để nhận một đơn thanh toán riêng.</p></div>
      <button type="button" onClick={() => { void config.refetch(); }} className="inline-flex items-center gap-2 text-xs font-semibold"><RefreshCw className="w-4 h-4" />Làm mới</button>
    </div>
    <div className="p-4">
      <p className="text-xs text-center text-stone-500">Trạng thái: {config.data?.status ?? (config.isError ? 'UNAVAILABLE' : 'Đang tải')}</p>
      <PayOSLinkQR staticUrl={config.data?.staticUrl} />
      {config.data && !config.data.ready && <p className="text-sm text-center text-amber-800">Chưa nhận đơn mới. Không dùng QR tài khoản cũ thay thế.</p>}
    </div>
      {/* Store WiFi Settings Section */}
      <div className="border-t border-stone-200/80 p-6 space-y-6 bg-stone-50/40">
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
          <div>
            <div className="flex items-center gap-2">
              <Wifi className="w-5 h-5 text-blue-600" />
              <h3 className="text-base font-bold text-stone-900">
                Cấu hình WiFi quán (Đồng bộ với Transaction Viewer)
              </h3>
            </div>
            <p className="text-xs text-stone-500 mt-1">
              Thiết lập Tên mạng và Mật khẩu WiFi để khách hàng quét mã QR kết nối tự động trên màn hình thu ngân / Transaction Viewer
            </p>
          </div>
        </div>

        {wifiNotice && (
          <div className="p-4 rounded-xl text-xs font-medium flex items-center justify-between gap-2 bg-emerald-50 text-emerald-800 border border-emerald-200">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="w-4 h-4 shrink-0 text-emerald-600" />
              <span>{wifiNotice}</span>
            </div>
            <button
              type="button"
              onClick={() => setWifiNotice(null)}
              className="text-stone-400 hover:text-stone-600 font-bold"
            >
              &times;
            </button>
          </div>
        )}

        <div className="grid grid-cols-1 md:grid-cols-2 gap-6 items-start">
          {/* WiFi Form */}
          <form onSubmit={handleSaveWifi} className="space-y-4 text-xs">
            <div>
              <label className="font-bold text-stone-800 block mb-1">Tên WiFi của quán (SSID)</label>
              <input
                type="text"
                value={wifiForm.ssid}
                placeholder="Ví dụ: ACB_Coffee_Free"
                onChange={(e) => setWifiForm((p) => ({ ...p, ssid: e.target.value }))}
                className="w-full px-3 py-2 bg-white border border-stone-200 rounded-xl text-xs text-stone-900 focus:outline-none focus:ring-2 focus:ring-blue-600/10 focus:border-blue-600"
              />
            </div>

            <div>
              <label className="font-bold text-stone-800 block mb-1">Mật khẩu WiFi</label>
              <input
                type="text"
                value={wifiForm.password || ''}
                placeholder="Để trống nếu mạng không cần mật khẩu"
                onChange={(e) => setWifiForm((p) => ({ ...p, password: e.target.value }))}
                className="w-full px-3 py-2 bg-white border border-stone-200 rounded-xl text-xs font-mono text-stone-900 focus:outline-none focus:ring-2 focus:ring-blue-600/10 focus:border-blue-600"
              />
            </div>

            <div className="grid grid-cols-2 gap-3 items-center">
              <div>
                <label className="font-bold text-stone-800 block mb-1">Chuẩn bảo mật</label>
                <select
                  value={wifiForm.security}
                  onChange={(e) =>
                    setWifiForm((p) => ({
                      ...p,
                      security: e.target.value as 'WPA' | 'WEP' | 'nopass',
                    }))
                  }
                  className="w-full px-2.5 py-2 bg-white border border-stone-200 rounded-xl text-xs text-stone-900 focus:outline-none focus:ring-2 focus:ring-blue-600/10 focus:border-blue-600"
                >
                  <option value="WPA">WPA / WPA2 (Mặc định)</option>
                  <option value="WEP">WEP</option>
                  <option value="nopass">Không có mật khẩu</option>
                </select>
              </div>

              <label className="flex items-center gap-2 pt-5 cursor-pointer select-none text-stone-700">
                <input
                  type="checkbox"
                  checked={Boolean(wifiForm.hidden)}
                  onChange={(e) => setWifiForm((p) => ({ ...p, hidden: e.target.checked }))}
                  className="rounded text-blue-600"
                />
                <span>Mạng WiFi ẩn</span>
              </label>
            </div>

            <div className="pt-2">
              <button
                type="submit"
                className="inline-flex items-center gap-1.5 px-4 py-2 rounded-xl text-xs font-semibold bg-blue-600 text-white hover:bg-blue-700 shadow-2xs transition cursor-pointer"
              >
                <Save className="w-3.5 h-3.5" />
                Lưu cấu hình WiFi
              </button>
            </div>
          </form>

          {/* WiFi QR Preview */}
          <div className="bg-white p-5 rounded-2xl border border-stone-200/80 flex flex-col items-center justify-center text-center">
            {wifiQRDataURL ? (
              <div className="space-y-3 w-full max-w-[240px]">
                <div className="bg-white p-3 rounded-2xl shadow-xs border border-stone-200">
                  <img
                    src={wifiQRDataURL}
                    alt="Mã QR WiFi"
                    className="w-full aspect-square object-contain rounded-xl"
                  />
                </div>
                <div className="text-xs space-y-1">
                  <p className="font-bold text-stone-900">{wifiForm.ssid}</p>
                  <p className="text-[11px] font-mono text-stone-500">
                    Pass: {wifiForm.password ? wifiForm.password : '(Không mật khẩu)'}
                  </p>
                  <p className="text-[10px] text-stone-400">
                    Quét bằng camera điện thoại để kết nối tự động
                  </p>
                </div>
              </div>
            ) : (
              <div className="py-8 space-y-2 text-stone-400">
                <Wifi className="w-12 h-12 mx-auto stroke-1" />
                <p className="text-xs font-medium text-stone-600">Chưa cấu hình WiFi quán</p>
                <p className="text-[11px] text-stone-400 max-w-[200px] mx-auto">
                  Nhập Tên mạng (SSID) và bấm "Lưu cấu hình WiFi" để tạo mã QR.
                </p>
              </div>
            )}
          </div>
        </div>
      </div>
  </div>;
};
