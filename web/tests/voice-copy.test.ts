import { describe, expect, it } from 'vitest';
import {
  ANNOUNCEMENT_TEMPLATES,
  buildBurstTransactionPhrase,
  buildSingleTransactionPhrase,
  DEFAULT_ANNOUNCEMENT_TEMPLATE,
  formatAnnouncementTemplate,
  sanitizeSpeechDescription,
  selectAnnouncementTemplate,
} from '../src/features/voice-announcements/voice-copy';

describe('voice-copy', () => {

  it('selects a stable template without replacing custom copy', () => {
    const ids = Array.from({ length: 30 }, (_, n) => `tx_local_${n}`);
    expect(new Set(ids.map((id) => selectAnnouncementTemplate(undefined, id))).size).toBeGreaterThan(1);
    expect(selectAnnouncementTemplate(undefined, '')).toBe(DEFAULT_ANNOUNCEMENT_TEMPLATE);
    expect(selectAnnouncementTemplate('  Nhận {amount}!  ', ids[0])).toBe('Nhận {amount}!');
    for (const id of ids) {
      expect(selectAnnouncementTemplate(undefined, id)).toBe(selectAnnouncementTemplate(DEFAULT_ANNOUNCEMENT_TEMPLATE, id));
    }
    for (const template of ANNOUNCEMENT_TEMPLATES) {
      expect(template.match(/\{amount\}/g)).toHaveLength(1);
    }
  });

  it('speaks each amount once and includes descriptions only when enabled', () => {
    for (const [id, amount, words] of [
      ['tx_local_1', '50000', 'năm mươi nghìn đồng'],
      ['tx_local_2', '500000', 'năm trăm nghìn đồng'],
      ['tx_local_3', '1250000', 'một triệu hai trăm năm mươi nghìn đồng'],
    ]) {
      const template = selectAnnouncementTemplate(undefined, id);
      const plain = buildSingleTransactionPhrase(amount, 'thử nghiệm', { template });
      expect(plain.split(words)).toHaveLength(2);
      expect(plain).not.toContain('thử nghiệm');
      expect(buildSingleTransactionPhrase(amount, 'thử nghiệm', { template, includeDescription: true })).toContain('thử nghiệm');
    }
  });

  it('interpolates {amount} token with spoken Vietnamese currency', () => {
    const res = formatAnnouncementTemplate('Cảm ơn quý khách đã gửi {amount}.', {
      amount: 100000,
    });
    expect(res).toBe('Cảm ơn quý khách đã gửi một trăm nghìn đồng.');
  });

  it('interpolates {amount_raw} token with unformatted numeric string', () => {
    const res = formatAnnouncementTemplate('Đã nhận {amount_raw} VND.', {
      amount: 50000,
    });
    expect(res).toBe('Đã nhận 50000 VND.');
  });

  it('interpolates both {amount} and {amount_raw} tokens', () => {
    const res = formatAnnouncementTemplate('Nhận {amount_raw} ({amount}).', {
      amount: 200000,
    });
    expect(res).toBe('Nhận 200000 (hai trăm nghìn đồng).');
  });

  it('interpolates {description} token when present and enabled', () => {
    const res = formatAnnouncementTemplate('Thanh toán từ {description} số tiền {amount}.', {
      amount: 20000,
      description: 'Cửa hàng cà phê',
      includeDescription: true,
    });
    expect(res).toBe('Thanh toán từ Cửa hàng cà phê số tiền hai mươi nghìn đồng.');
  });

  it('omits {description} token and cleans label when description is disabled or empty', () => {
    const res = formatAnnouncementTemplate('Đã nhận {amount}. Nội dung: {description}.', {
      amount: 20000,
      description: 'Loi nhan',
      includeDescription: false,
    });
    expect(res).toBe('Đã nhận hai mươi nghìn đồng.');

    const emptyDesc = formatAnnouncementTemplate('Đã nhận {amount}. Nội dung: {description}.', {
      amount: 20000,
      description: '',
      includeDescription: true,
    });
    expect(emptyDesc).toBe('Đã nhận hai mươi nghìn đồng.');
  });

  it('falls back to default template when empty or whitespace provided', () => {
    const res = formatAnnouncementTemplate('   ', {
      amount: 10000,
    });
    expect(res).toBe('Đa tạ quý khách vì mười nghìn đồng.');
  });

  it('sanitizes description correctly', () => {
    const raw = 'NGUYEN VAN A chuyen tien https://example.com/tx/123 @#$ test';
    const sanitized = sanitizeSpeechDescription(raw);
    expect(sanitized).not.toContain('https://');
    expect(sanitized).not.toContain('@#$');
    expect(sanitized).toBe('NGUYEN VAN A chuyen tien test');
  });

  it('builds burst transaction phrase', () => {
    const phrase = buildBurstTransactionPhrase(5, 3200000);
    expect(phrase).toBe('Bạn vừa nhận được 5 giao dịch mới, tổng cộng ba triệu hai trăm nghìn đồng.');
  });
});
