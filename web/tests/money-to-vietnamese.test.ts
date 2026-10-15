import { describe, expect, it } from 'bun:test';
import { speakVnd } from '../src/features/voice-announcements/money-to-vietnamese';
import fixtures from '../../internal/voicecopy/catalog/amount-fixtures.v1.json';

describe('speakVnd', () => {
  it('handles zero correctly', () => {
    expect(speakVnd(0)).toBe('không đồng');
    expect(speakVnd('0')).toBe('không đồng');
    expect(speakVnd('0000')).toBe('không đồng');
  });

  it('reads hundreds of thousands correctly', () => {
    expect(speakVnd(500000)).toBe('năm trăm nghìn đồng');
    expect(speakVnd('500000')).toBe('năm trăm nghìn đồng');
    expect(speakVnd('500.000')).toBe('năm trăm nghìn đồng');
  });

  it('reads millions correctly', () => {
    expect(speakVnd(1250000)).toBe('một triệu hai trăm năm mươi nghìn đồng');
    expect(speakVnd(10000000)).toBe('mười triệu đồng');
    expect(speakVnd(10500000)).toBe('mười triệu năm trăm nghìn đồng');
    expect(speakVnd('1.250.000')).toBe('một triệu hai trăm năm mươi nghìn đồng');
  });

  it('reads billions correctly', () => {
    expect(speakVnd(1000000000)).toBe('một tỷ đồng');
    expect(speakVnd(2500000000)).toBe('hai tỷ năm trăm triệu đồng');
  });

  it('handles irregular Vietnamese numbers like mốt, lăm, linh', () => {
    expect(speakVnd(21000)).toBe('hai mươi mốt nghìn đồng');
    expect(speakVnd(25000)).toBe('hai mươi lăm nghìn đồng');
    expect(speakVnd(15000)).toBe('mười lăm nghìn đồng');
    expect(speakVnd(105000)).toBe('một trăm linh năm nghìn đồng');
  });

  it('matches all 24 canonical fixtures identically with Go formatter', () => {
    for (const f of fixtures) {
      expect(speakVnd(f.amount)).toBe(f.spoken);
    }
  });
});
