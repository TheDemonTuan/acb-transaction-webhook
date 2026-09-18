import { isPublicViewerHost } from './app/runtime-mode';

export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;

  constructor(message: string, status: number, code?: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

export const AUTH_CODE_SUPERSEDED = 'AUTH_SESSION_SUPERSEDED';
export const AUTH_CODE_NOT_FOUND = 'AUTH_SESSION_NOT_FOUND';
export const AUTH_CODE_UNAVAILABLE = 'AUTH_SESSION_UNAVAILABLE';
export const CSRF_CODE_ORIGIN_MISMATCH = 'ORIGIN_MISMATCH';
export const CSRF_CODE_TOKEN_INVALID = 'CSRF_TOKEN_INVALID';

export const TERMINAL_AUTH_CODES = [AUTH_CODE_SUPERSEDED, AUTH_CODE_NOT_FOUND] as const;
export type TerminalAuthCode = (typeof TERMINAL_AUTH_CODES)[number];

export const isTerminalAuthError = (error: unknown): error is ApiError => {
  if (!(error instanceof ApiError)) return false;
  return error.code === AUTH_CODE_SUPERSEDED || error.code === AUTH_CODE_NOT_FOUND;
};

export const parseApiError = async (response: Response): Promise<ApiError> => {
  const contentType = response.headers.get('content-type')?.toLowerCase() ?? '';
  let message = `Máy chủ trả về lỗi HTTP ${response.status}. Vui lòng thử lại.`;
  let code: string | undefined;

  if (contentType.includes('application/json')) {
    try {
      const payload = (await response.json()) as { error?: unknown; code?: unknown };
      if (typeof payload.error === 'string' && payload.error.trim()) {
        message = payload.error.trim();
      }
      if (typeof payload.code === 'string' && payload.code.trim()) {
        code = payload.code.trim();
      }
    } catch {
      // Use the bounded HTTP status message.
    }
  }

  if (response.status === 429) {
    message = 'Có quá nhiều yêu cầu cùng lúc. Vui lòng đợi một chút rồi thử lại.';
  } else if (code === CSRF_CODE_ORIGIN_MISMATCH) {
    message = 'Xác thực bảo mật Origin không khớp với cấu hình máy chủ. Vui lòng kiểm tra PUBLIC_ORIGIN.';
  } else if (code === CSRF_CODE_TOKEN_INVALID) {
    message = 'Phiên bảo mật (CSRF) không hợp lệ hoặc đã hết hạn. Vui lòng thử lại.';
  }

  return new ApiError(message, response.status, code);
};

export const apiErrorMessage = async (response: Response): Promise<string> => {
  const err = await parseApiError(response);
  return err.message;
};

export const isMutation = (method: string): boolean => {
  const m = method.toUpperCase();
  return m === 'POST' || m === 'PUT' || m === 'PATCH' || m === 'DELETE';
};

let cachedCsrfToken: string | null = null;
let csrfPromise: Promise<string> | null = null;

export const getCsrfToken = async (forceRefresh = false): Promise<string> => {
  if (forceRefresh) {
    cachedCsrfToken = null;
  }
  if (!forceRefresh && cachedCsrfToken) {
    return cachedCsrfToken;
  }
  if (csrfPromise) {
    return csrfPromise;
  }
  csrfPromise = (async () => {
    try {
      const res = await api<{ token: string }>('/csrf');
      cachedCsrfToken = res.token;
      return res.token;
    } finally {
      csrfPromise = null;
    }
  })();
  return csrfPromise;
};

export const api = async <T,>(path: string, init?: RequestInit): Promise<T> => {
  const method = init?.method?.toUpperCase() ?? 'GET';
  const mutating = isMutation(method);

  if (isPublicViewerHost() && mutating) {
    throw new ApiError(
      'Trang xem giao dịch chỉ hỗ trợ đọc dữ liệu.',
      405,
      'PUBLIC_READ_ONLY',
    );
  }

  const basePath = isPublicViewerHost() ? '/api/public/v1' : '/api/v1';
  const headers = new Headers(init?.headers);

  if (mutating && !isPublicViewerHost()) {
    const token = await getCsrfToken();
    if (!headers.has('X-CSRF-Token')) {
      headers.set('X-CSRF-Token', token);
    }
  }

  let response: Response;
  try {
    response = await fetch(`${basePath}${path}`, { credentials: 'same-origin', ...init, headers });
  } catch (err: any) {
    if ((err instanceof DOMException && err.name === 'AbortError') || err?.name === 'AbortError') {
      throw err;
    }
    throw new Error('Không thể kết nối máy chủ. Vui lòng kiểm tra kết nối và thử lại.');
  }

  if (!response.ok) {
    const error = await parseApiError(response);
    if (mutating && !isPublicViewerHost() && error.code === CSRF_CODE_TOKEN_INVALID) {
      const newToken = await getCsrfToken(true);
      headers.set('X-CSRF-Token', newToken);
      try {
        response = await fetch(`${basePath}${path}`, { credentials: 'same-origin', ...init, headers });
      } catch (err: any) {
        if ((err instanceof DOMException && err.name === 'AbortError') || err?.name === 'AbortError') {
          throw err;
        }
        throw new Error('Không thể kết nối máy chủ. Vui lòng kiểm tra kết nối và thử lại.');
      }
      if (!response.ok) {
        throw await parseApiError(response);
      }
    } else {
      throw error;
    }
  }

  try {
    return (await response.json()) as T;
  } catch {
    throw new Error('Máy chủ trả về dữ liệu không hợp lệ. Vui lòng thử lại.');
  }
};

export interface AudioResponseResult {
  data: ArrayBuffer;
  provider: string;
  voice: string;
  fallback: boolean;
  cached: boolean;
}

export const apiAudio = async (path: string, init?: RequestInit): Promise<AudioResponseResult> => {
  if (isPublicViewerHost()) {
    throw new ApiError(
      'Trang xem giao dịch chỉ hỗ trợ đọc dữ liệu.',
      405,
      'PUBLIC_READ_ONLY',
    );
  }

  const method = init?.method?.toUpperCase() ?? 'GET';
  const isMutating = isMutation(method);
  let token: string | null = null;
  if (isMutating) {
    try {
      token = await getCsrfToken();
    } catch {}
  }

  const headers = new Headers(init?.headers);
  if (token && !headers.has('X-CSRF-Token')) {
    headers.set('X-CSRF-Token', token);
  }

  let response: Response;
  try {
    response = await fetch(`/api/v1${path}`, { credentials: 'same-origin', ...init, headers });
  } catch (err: any) {
    if ((err instanceof DOMException && err.name === 'AbortError') || err?.name === 'AbortError') {
      throw err;
    }
    throw new Error('Không thể kết nối máy chủ. Vui lòng kiểm tra kết nối và thử lại.');
  }

  if (!response.ok && isMutating) {
    let code: string | undefined;
    try {
      const cloned = response.clone();
      const body = await cloned.json();
      code = body?.code;
    } catch {}

    if (code === CSRF_CODE_TOKEN_INVALID) {
      token = await getCsrfToken(true);
      headers.set('X-CSRF-Token', token);
      response = await fetch(`/api/v1${path}`, { credentials: 'same-origin', ...init, headers });
    }
  }

  if (!response.ok) {
    throw await parseApiError(response);
  }

  const data = await response.arrayBuffer();
  return {
    data,
    provider: response.headers.get('X-TTS-Provider') || 'edge',
    voice: response.headers.get('X-TTS-Voice') || '',
    fallback: response.headers.get('X-TTS-Fallback') === 'true',
    cached: response.headers.get('X-TTS-Cached') === 'true',
  };
};

export const invalidateCsrfToken = () => {
  cachedCsrfToken = null;
};
