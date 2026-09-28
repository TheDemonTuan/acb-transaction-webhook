import QRCode from 'qrcode';

export interface WifiSettings {
  ssid: string;
  password?: string;
  security: 'WPA' | 'WEP' | 'nopass';
  hidden?: boolean;
}

export const WIFI_STORAGE_KEY = 'viewer_wifi_settings';

export const DEFAULT_WIFI_SETTINGS: WifiSettings = {
  ssid: '',
  password: '',
  security: 'WPA',
  hidden: false,
};

/**
 * Escape special characters per ZXing / WPA WiFi barcode specification:
 * Backslash, semicolon, comma, colon, quote.
 */
export function escapeWifiString(str: string): string {
  return str.replace(/([\\;,:"'])/g, '\\$1');
}

/**
 * Builds standard WiFi QR format string:
 * WIFI:S:<SSID>;T:<WPA|WEP|nopass>;P:<PASSWORD>;H:<true|false>;;
 */
export function buildWifiQRString(
  ssid: string,
  password = '',
  security: 'WPA' | 'WEP' | 'nopass' = 'WPA',
  hidden = false
): string {
  const cleanSsid = ssid.trim();
  if (!cleanSsid) return '';
  const sec = security || (password ? 'WPA' : 'nopass');
  let qr = `WIFI:S:${escapeWifiString(cleanSsid)};T:${sec};`;
  if (sec !== 'nopass' && password) {
    qr += `P:${escapeWifiString(password)};`;
  }
  if (hidden) {
    qr += 'H:true;';
  }
  qr += ';';
  return qr;
}
function obfuscateSecret(val: string): string {
  if (!val) return '';
  try {
    return btoa(unescape(encodeURIComponent(val)));
  } catch {
    return val;
  }
}

function revealSecret(val: string): string {
  if (!val) return '';
  try {
    return decodeURIComponent(escape(atob(val)));
  } catch {
    return val;
  }
}

export function loadWifiSettings(): WifiSettings {
  if (typeof window === 'undefined' || !window.localStorage) {
    return { ...DEFAULT_WIFI_SETTINGS };
  }
  try {
    const raw = window.localStorage.getItem(WIFI_STORAGE_KEY);
    if (!raw) return { ...DEFAULT_WIFI_SETTINGS };
    const parsed = JSON.parse(raw);
    const secretVal =
      typeof parsed.authBlob === 'string'
        ? revealSecret(parsed.authBlob)
        : typeof parsed.password === 'string'
          ? parsed.password
          : '';
    return {
      ssid: typeof parsed.ssid === 'string' ? parsed.ssid : '',
      password: secretVal,
      security: parsed.security === 'WEP' || parsed.security === 'nopass' ? parsed.security : 'WPA',
      hidden: Boolean(parsed.hidden),
    };
  } catch {
    return { ...DEFAULT_WIFI_SETTINGS };
  }
}

export function saveWifiSettings(settings: Partial<WifiSettings>): WifiSettings {
  const current = loadWifiSettings();
  const next: WifiSettings = {
    ssid: settings.ssid !== undefined ? settings.ssid : current.ssid,
    password: settings.password !== undefined ? settings.password : current.password,
    security: settings.security !== undefined ? settings.security : current.security,
    hidden: settings.hidden !== undefined ? settings.hidden : current.hidden,
  };
  const storagePayload = {
    ssid: next.ssid,
    authBlob: obfuscateSecret(next.password || ''),
    security: next.security,
    hidden: next.hidden,
  };
  if (typeof window !== 'undefined' && window.localStorage) {
    try {
      window.localStorage.setItem(WIFI_STORAGE_KEY, JSON.stringify(storagePayload));
    } catch {}
  }
  return next;
}
export async function generateWifiQRDataURL(
  wifiString: string,
  margin = 2,
  width = 320
): Promise<string> {
  if (!wifiString) return '';
  return QRCode.toDataURL(wifiString, {
    errorCorrectionLevel: 'M',
    margin,
    width,
    color: {
      dark: '#1c1917', // stone-900
      light: '#ffffff',
    },
  });
}
