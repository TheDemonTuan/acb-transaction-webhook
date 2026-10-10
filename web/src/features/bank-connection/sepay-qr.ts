import jsQR from 'jsqr';

export const QR_IMAGE_MAX_BYTES = 8 * 1024 * 1024;
const MAX_SIDE = 4096;
const MAX_PIXELS = 16_000_000;

export interface SePayQRReceiver {
  bin: string;
  accountNumber: string;
  bankCode?: string;
}

// This intentionally small allowlist is NAPAS BIN -> canonical bank code, not a guess from a bank logo/name.
const BANK_CODES: Readonly<Record<string, string>> = {
  '970416': 'ACB', '970452': 'KienlongBank', '970436': 'VCB', '970418': 'BIDV',
  '970415': 'VietinBank', '970405': 'Agribank', '970422': 'MB', '970407': 'Techcombank',
};

const tlv = (raw: string): Map<string, string> | null => {
  const fields = new Map<string, string>();
  let offset = 0;
  while (offset < raw.length) {
    const tag = raw.slice(offset, offset + 2);
    const lengthText = raw.slice(offset + 2, offset + 4);
    if (!/^\d{2}$/.test(tag) || !/^\d{2}$/.test(lengthText) || fields.has(tag)) return null;
    const length = Number(lengthText);
    if (offset + 4 + length > raw.length) return null;
    fields.set(tag, raw.slice(offset + 4, offset + 4 + length));
    offset += 4 + length;
  }
  return fields;
};

const crc16 = (raw: string): string => {
  let crc = 0xffff;
  for (const byte of new TextEncoder().encode(raw)) {
    crc ^= byte << 8;
    for (let bit = 0; bit < 8; bit++) crc = crc & 0x8000 ? ((crc << 1) ^ 0x1021) & 0xffff : (crc << 1) & 0xffff;
  }
  return crc.toString(16).toUpperCase().padStart(4, '0');
};

// Only extracts the known VietQR/NAPAS transfer structure. Never modifies or regenerates the payload.
export function extractSePayQRReceiver(payload: string): SePayQRReceiver | null {
  if (!/^[\x20-\x7e]+$/.test(payload)) return null;
  const root = tlv(payload);
  if (!root || root.get('00') !== '01' || root.get('53') !== '704' || root.get('58') !== 'VN' ||
      !/6304[\dA-Fa-f]{4}$/.test(payload) || root.get('63')?.toUpperCase() !== crc16(payload.slice(0, -4))) return null;
  const merchant = tlv(root.get('38') ?? '');
  if (!merchant || merchant.get('00') !== 'A000000727' || merchant.get('02') !== 'QRIBFTTA') return null;
  const receiver = tlv(merchant.get('01') ?? '');
  const bin = receiver?.get('00') ?? '';
  const accountNumber = receiver?.get('01') ?? '';
  if (!/^\d{6}$/.test(bin) || !/^[a-zA-Z0-9]{1,64}$/.test(accountNumber)) return null;
  return { bin, accountNumber, ...(BANK_CODES[bin] ? { bankCode: BANK_CODES[bin] } : {}) };
}

export function validateSePayQRPayload(payload: string): void {
  if (!payload || new TextEncoder().encode(payload).length > 4096 || /[\u0000-\u001f\u007f]/.test(payload)) {
    throw new Error('Nội dung QR trống, quá 4096 byte hoặc chứa ký tự điều khiển. Hãy xuất lại QR gốc từ SePay Store.');
  }
  if (/^[a-z][a-z\d+.-]*:/i.test(payload.trim()) || /^(?:www\.|\/\/)/i.test(payload.trim())) {
    throw new Error('Đây là QR liên kết, không phải QR chuyển khoản. Chọn QR gốc của Store để quét bằng ứng dụng ngân hàng.');
  }
}

export async function decodeSePayQRImage(file: File): Promise<{ payload: string; receiver: SePayQRReceiver | null }> {
  if (!['image/png', 'image/jpeg', 'image/webp'].includes(file.type)) {
    throw new Error('Chọn ảnh PNG, JPEG hoặc WebP. Không nhận SVG, PDF hay ảnh động.');
  }
  if (!file.size || file.size > QR_IMAGE_MAX_BYTES) throw new Error('Ảnh phải có dung lượng từ 1 byte đến 8 MB. Hãy xuất ảnh QR rõ nét, không dùng ảnh chụp quá lớn.');
  const url = URL.createObjectURL(file);
  try {
    const image = new Image();
    image.src = url;
    try { await image.decode(); } catch { throw new Error('Không đọc được ảnh. Hãy xuất lại ảnh PNG từ SePay Store.'); }
    const width = image.naturalWidth;
    const height = image.naturalHeight;
    if (!width || !height || width > MAX_SIDE || height > MAX_SIDE || width * height > MAX_PIXELS) {
      throw new Error('Ảnh vượt giới hạn 4096 px mỗi chiều hoặc 16 triệu điểm ảnh. Cắt riêng một QR rồi thử lại.');
    }
    const canvas = document.createElement('canvas');
    canvas.width = width;
    canvas.height = height;
    const ctx = canvas.getContext('2d', { willReadFrequently: true });
    if (!ctx) throw new Error('Trình duyệt không đọc được ảnh QR. Hãy dùng trình duyệt hiện hành và thử lại.');
    ctx.fillStyle = '#fff';
    ctx.fillRect(0, 0, width, height);
    ctx.drawImage(image, 0, 0);
    const pixels = ctx.getImageData(0, 0, width, height);
    // jsQR works offline on all supported browsers, including those without BarcodeDetector.
    const code = jsQR(pixels.data, width, height, { inversionAttempts: 'attemptBoth' });
    if (!code) throw new Error('Không tìm thấy QR. Chọn ảnh rõ nét có đầy đủ bốn góc và viền trắng, không bị cắt hoặc lóa.');
    // Mask the first symbol only, then detect again to reject ambiguous multi-QR images.
    const corners = [code.location.topLeftCorner, code.location.topRightCorner, code.location.bottomRightCorner, code.location.bottomLeftCorner];
    ctx.beginPath();
    ctx.moveTo(corners[0].x, corners[0].y);
    for (const corner of corners.slice(1)) ctx.lineTo(corner.x, corner.y);
    ctx.closePath();
    ctx.fillStyle = '#fff';
    ctx.fill();
    ctx.lineWidth = 3;
    ctx.strokeStyle = '#fff';
    ctx.stroke();
    const remaining = ctx.getImageData(0, 0, width, height);
    if (jsQR(remaining.data, width, height, { inversionAttempts: 'attemptBoth' })) {
      throw new Error('Ảnh có nhiều QR. Cắt riêng đúng QR chuyển khoản của Store để tránh chọn nhầm người nhận.');
    }
    validateSePayQRPayload(code.data);
    return { payload: code.data, receiver: extractSePayQRReceiver(code.data) };
  } finally { URL.revokeObjectURL(url); }
}
