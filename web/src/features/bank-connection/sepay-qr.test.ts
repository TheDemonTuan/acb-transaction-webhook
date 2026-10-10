import { describe, expect, it } from 'vitest';
import { extractSePayQRReceiver, validateSePayQRPayload } from './sepay-qr';

const transferFixture = (bin = '970436', account = '0000000000'): string => {
  const field = (tag: string, value: string) => `${tag}${String(value.length).padStart(2, '0')}${value}`;
  const merchant = field('00', 'A000000727') + field('01', field('00', bin) + field('01', account)) + field('02', 'QRIBFTTA');
  const raw = field('00', '01') + field('01', '11') + field('38', merchant) + field('53', '704') + field('58', 'VN') + '6304';
  let crc = 0xffff;
  for (const byte of new TextEncoder().encode(raw)) {
    crc ^= byte << 8;
    for (let bit = 0; bit < 8; bit++) crc = crc & 0x8000 ? ((crc << 1) ^ 0x1021) & 0xffff : (crc << 1) & 0xffff;
  }
  return raw + crc.toString(16).toUpperCase().padStart(4, '0');
};

describe('SePay offline receiver extraction', () => {
  it('extracts only verified NAPAS receiver fields without modifying opaque payload', () => {
    const payload = transferFixture();
    expect(extractSePayQRReceiver(payload)).toEqual({ bin: '970436', accountNumber: '0000000000', bankCode: 'VCB' });
    expect(() => validateSePayQRPayload(payload)).not.toThrow();
    expect(payload).toBe(transferFixture());
  });
  it('does not invent a bank for an unknown BIN', () => {
    expect(extractSePayQRReceiver(transferFixture('999999'))).toEqual({ bin: '999999', accountNumber: '0000000000' });
  });
  it('does not prefill receiver from bad CRC, duplicate or malformed TLV', () => {
    const payload = transferFixture();
    expect(extractSePayQRReceiver(payload.slice(0, -4) + '0000')).toBeNull();
    expect(extractSePayQRReceiver('000201000201' + payload)).toBeNull();
    expect(extractSePayQRReceiver('3899' + payload)).toBeNull();
    expect(extractSePayQRReceiver('https://example.test/pay')).toBeNull();
  });
  it.each(['https://example.test/pay', ' HTTPS://example.test/pay ', 'javascript:alert(1)', '//example.test', 'www.example.test', '', 'a\u0000b', 'x'.repeat(4097)])('rejects unsupported payload %j', payload => {
    expect(() => validateSePayQRPayload(payload)).toThrow();
  });
});
