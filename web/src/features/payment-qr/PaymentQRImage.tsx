import React, { useEffect, useState } from 'react';
import QRCode from 'qrcode';
import { Copy, Download } from 'lucide-react';

export const PaymentQRImage: React.FC<{ payload: string; alt: string; downloadable?: boolean }> = ({ payload, alt, downloadable = false }) => {
  const [image, setImage] = useState('');
  const [error, setError] = useState(false);
  useEffect(() => {
    let active = true;
    setImage('');
    setError(false);
    if (payload) {
      void QRCode.toDataURL(payload, { width: 640, margin: 2, errorCorrectionLevel: 'M' }).then(
        (value) => { if (active) setImage(value); },
        () => { if (active) setError(true); },
      );
    }
    return () => { active = false; };
  }, [payload]);
  return <div className="flex flex-col items-center gap-3">
    {image ? <img src={image} alt={alt} className="w-64 max-w-full aspect-square object-contain rounded-2xl bg-white" /> :
      <p role="status" className="p-8 text-sm text-stone-500">{error ? 'Không thể tạo ảnh QR. Kiểm tra lại thông tin nhận tiền trước khi thanh toán.' : 'Đang tạo ảnh QR…'}</p>}
    {downloadable && image && <a href={image} download="payment-url-qr.png" className="inline-flex items-center gap-2 text-sm font-semibold text-emerald-700"><Download className="w-4 h-4" />Tải ảnh QR</a>}
  </div>;
};

export const PayOSLinkQR: React.FC<{ staticUrl?: string }> = ({ staticUrl }) => {
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState(false);
  return <section className="flex flex-col items-center text-center gap-4 p-4">
    <h3 className="font-bold text-stone-900">Liên kết nhập tiền payOS</h3>
    <p className="text-sm text-stone-600">Quét bằng camera để mở trang nhập tiền</p>
    <p className="text-xs text-stone-500">Mã này mở trang nhập VND, không quét trực tiếp trong ứng dụng ngân hàng.</p>
    {staticUrl ? <>
      <PaymentQRImage payload={staticUrl} alt="Liên kết nhập tiền payOS" downloadable />
      <a href={staticUrl} target="_blank" rel="noreferrer" className="break-all text-sm text-emerald-700">{staticUrl}</a>
      <button type="button" onClick={() => {
        void navigator.clipboard.writeText(staticUrl).then(() => { setCopied(true); setCopyError(false); }, () => setCopyError(true));
      }} className="min-h-11 inline-flex items-center gap-2 px-4 py-2 rounded-xl border border-stone-200 text-sm"><Copy className="w-4 h-4" />{copied ? 'Đã sao chép liên kết' : 'Sao chép liên kết'}</button>
      {copyError && <p role="alert" className="text-sm text-rose-700">Không thể sao chép. Hãy chọn liên kết phía trên.</p>}
    </> : <p role="status" className="text-sm text-stone-500">Đang chờ URL thanh toán từ máy chủ.</p>}
  </section>;
};
