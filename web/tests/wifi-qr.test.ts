import { describe, it, expect, beforeEach } from 'vitest';
import {
  escapeWifiString,
  buildWifiQRString,
  loadWifiSettings,
  saveWifiSettings,
  generateWifiQRDataURL,
  WIFI_STORAGE_KEY,
} from '../src/features/payment-qr/wifi-qr';

describe('WiFi QR Utilities', () => {
  let store: Record<string, string> = {};

  beforeEach(() => {
    store = {};
    (globalThis as unknown as { window: unknown }).window = {
      localStorage: {
        getItem: (k: string) => store[k] ?? null,
        setItem: (k: string, v: string) => {
          store[k] = v;
        },
        removeItem: (k: string) => {
          delete store[k];
        },
      },
    };
  });
  describe('escapeWifiString', () => {
    it('escapes backslash, semicolon, comma, colon, and quotes per ZXing spec', () => {
      expect(escapeWifiString('NormalSSID')).toBe('NormalSSID');
      expect(escapeWifiString('My;WiFi:Network,2"4\\G')).toBe('My\\;WiFi\\:Network\\,2\\"4\\\\G');
    });
  });

  describe('buildWifiQRString', () => {
    it('returns empty string if ssid is empty or only whitespace', () => {
      expect(buildWifiQRString('')).toBe('');
      expect(buildWifiQRString('   ')).toBe('');
    });

    it('builds standard WPA WiFi QR string with password', () => {
      const qr = buildWifiQRString('ACB_Coffee', 'mock-wifi-key', 'WPA');
      expect(qr).toBe('WIFI:S:ACB_Coffee;T:WPA;P:mock-wifi-key;;');
    });

    it('builds nopass WiFi QR string when security is nopass or password empty', () => {
      const qr = buildWifiQRString('ACB_Free_WiFi', '', 'nopass');
      expect(qr).toBe('WIFI:S:ACB_Free_WiFi;T:nopass;;');
    });

    it('supports hidden network flag', () => {
      const qr = buildWifiQRString('Hidden_Staff', 'mock-hidden-pass', 'WPA', true);
      expect(qr).toBe('WIFI:S:Hidden_Staff;T:WPA;P:mock-hidden-pass;H:true;;');
    });

    it('escapes special characters inside SSID and password', () => {
      const qr = buildWifiQRString('Café:Corner;1', 'pass;word:123', 'WPA');
      expect(qr).toBe('WIFI:S:Café\\:Corner\\;1;T:WPA;P:pass\\;word\\:123;;');
    });
  });

  describe('loadWifiSettings & saveWifiSettings', () => {
    it('loads defaults when localStorage is empty', () => {
      const s = loadWifiSettings();
      expect(s.ssid).toBe('');
      expect(s.password).toBe('');
      expect(s.security).toBe('WPA');
      expect(s.hidden).toBe(false);
    });

    it('saves and loads settings from localStorage', () => {
      saveWifiSettings({
        ssid: 'Store_WiFi',
        password: 'mock-stored-pass',
        security: 'WPA',
      });
      const s = loadWifiSettings();
      expect(s.ssid).toBe('Store_WiFi');
      expect(s.password).toBe('mock-stored-pass');
      expect(s.security).toBe('WPA');
      expect(s.hidden).toBe(false);
    });

    it('partially updates settings preserving existing values', () => {
      saveWifiSettings({ ssid: 'First_SSID', password: 'pass1' });
      saveWifiSettings({ password: 'new_pass' });
      const s = loadWifiSettings();
      expect(s.ssid).toBe('First_SSID');
      expect(s.password).toBe('new_pass');
    });
  });

  describe('generateWifiQRDataURL', () => {
    it('returns empty string if wifiString is empty', async () => {
      const dataUrl = await generateWifiQRDataURL('');
      expect(dataUrl).toBe('');
    });

    it('generates a valid data:image/png base64 URL for valid WiFi string', async () => {
      const wifiStr = buildWifiQRString('ACB_Coffee', 'mock-wifi-key');
      const dataUrl = await generateWifiQRDataURL(wifiStr);
      expect(dataUrl.startsWith('data:image/png;base64,')).toBe(true);
    });
  });
});
