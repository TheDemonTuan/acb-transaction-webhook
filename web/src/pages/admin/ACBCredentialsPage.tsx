import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { ApiError, api, invalidateCsrfToken } from '../../api';

interface CredentialGrantView {
  revision: number;
  usernameMasked: string;
  accountMasked: string;
  expiresAt: string;
  canSave: boolean;
  blockedReason: string;
}

interface CredentialSaveResult {
  saved: boolean;
  revision: number;
  requiresLogin: boolean;
}

type Phase = 'checking' | 'ready' | 'saving' | 'validation-error' | 'closed' | 'unknown' | 'saved';

const inputClass = 'w-full rounded-xl border border-stone-300 bg-white px-3.5 py-3 text-base text-stone-900 focus:outline-none focus:ring-2 focus:ring-emerald-500 focus:border-emerald-500 disabled:bg-stone-100';
const buttonClass = 'min-h-11 rounded-xl border border-stone-300 px-4 py-2.5 text-sm font-semibold text-stone-700 hover:bg-stone-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 disabled:opacity-50 disabled:cursor-not-allowed';

function errorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401 || error.status === 403) {
      if (error.code === 'CSRF_TOKEN_INVALID') {
        return 'Phiên bảo mật đã hết hạn. Hãy kiểm tra lại liên kết trước khi lưu.';
      }
      if (error.code === 'ORIGIN_MISMATCH') {
        return 'Địa chỉ trang không khớp cấu hình bảo mật. Hãy mở đúng liên kết bot gửi; nếu vẫn lỗi, liên hệ người quản trị VPS.';
      }
      return 'Cần đăng nhập Cloudflare Access bằng tài khoản OWNER. Nếu đang dùng trình duyệt trong Telegram, hãy mở liên kết trong trình duyệt thông thường, đăng nhập rồi xin liên kết mới từ bot.';
    }
    if (error.status === 410 || error.code === 'CREDENTIAL_GRANT_EXPIRED') {
      return 'Liên kết đã hết hạn, đã được sử dụng hoặc đã bị thu hồi. Hãy xin liên kết mới qua mục Đổi thông tin đăng nhập trong Telegram.';
    }
    if (error.code === 'CREDENTIALS_REVISION_CONFLICT') {
      return 'Trạng thái ACB hoặc thông tin đã lưu đã thay đổi. Hãy xin liên kết mới trong Telegram; trang này sẽ không ghi đè phiên bản mới.';
    }
    if (error.code === 'ACB_SESSION_BUSY') {
      return 'ACB đang có phiên hoặc thao tác đang chạy. Hãy dùng Đăng xuất ACB trong Telegram nếu phiên còn hoạt động; chờ hoàn tất hoặc hủy lượt đăng nhập đang chạy, rồi kiểm tra lại tại đây.';
    }
    if (error.code === 'INVALID_CREDENTIAL_INPUT') {
      return 'Thông tin chưa hợp lệ. Hãy nhập đầy đủ tên đăng nhập và mật khẩu, xác nhận mật khẩu giống nhau; không dùng ký tự xuống dòng hoặc NUL.';
    }
    if (error.code === 'CREDENTIALS_UNAVAILABLE') {
      return 'Kho thông tin đăng nhập tạm thời không khả dụng. Dữ liệu chưa được thay đổi. Hãy kiểm tra lại sau hoặc liên hệ người quản trị VPS.';
    }
    if (error.code === 'MUTATION_GATE_LOCKED') {
      return 'Hệ thống đang bảo trì và chưa lưu thay đổi. Hãy chờ bảo trì hoàn tất rồi kiểm tra lại quyền lưu; nếu liên kết hết hạn, xin liên kết mới trong Telegram.';
    }
    if (error.status === 429) {
      return 'Đã kiểm tra liên kết quá nhiều lần. Hãy đợi một phút trước khi kiểm tra lại; nếu liên kết hết hạn, xin liên kết mới trong Telegram.';
    }
    if (error.code === 'CSRF_TOKEN_INVALID') {
      return 'Phiên bảo mật đã hết hạn. Hãy kiểm tra lại liên kết trước khi lưu.';
    }
    return 'Yêu cầu bị máy chủ từ chối. Hãy xem trạng thái trong Telegram; nếu đang bảo trì, chờ hoàn tất rồi kiểm tra lại.';
  }
  return 'Không thể xác minh liên kết do lỗi kết nối hoặc phản hồi không hợp lệ. Hãy kiểm tra kết nối rồi kiểm tra lại; trang không tự gửi lại yêu cầu.';
}


// Abort also bounds the wait for api()'s CSRF request. No timed-out save is replayed.
async function credentialRequest<T>(path: string, body: object, controller: AbortController): Promise<T> {
  let timer: number | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = window.setTimeout(() => {
      controller.abort();
      reject(new Error('CREDENTIAL_REQUEST_TIMEOUT'));
    }, 30_000);
  });
  try {
    return await Promise.race([
      api<T>(path, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
        cache: 'no-store',
        signal: controller.signal,
      }, { retryCsrf: false }),
      timeout,
    ]);
  } finally {
    window.clearTimeout(timer);
  }
}

export const ACBCredentialsPage: React.FC = () => {
  const location = useLocation();
  const navigate = useNavigate();
  const fragmentRead = useRef(false);
  const saveInFlight = useRef(false);
  const saveController = useRef<AbortController | null>(null);
  const mounted = useRef(false);
  const [grant, setGrant] = useState('');
  const [view, setView] = useState<CredentialGrantView | null>(null);
  const [phase, setPhase] = useState<Phase>('checking');
  const [message, setMessage] = useState('');
  const [validationRun, setValidationRun] = useState(0);
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [showPasswords, setShowPasswords] = useState(false);

  useLayoutEffect(() => {
    // The ref keeps StrictMode's effect replay from treating the removed fragment as a new visit.
    if (fragmentRead.current) return;
    fragmentRead.current = true;
    const params = new URLSearchParams(window.location.hash.slice(1));
    const tokens = params.getAll('grant');
    window.history.replaceState(window.history.state, '', window.location.pathname + window.location.search);
    if (tokens.length !== 1 || !/^[A-Za-z0-9_-]{43}$/.test(tokens[0])) {
      setPhase('closed');
      setMessage('Không có liên kết cấp quyền hợp lệ. Hãy chọn Đổi thông tin đăng nhập trong Telegram để nhận liên kết mới. Tải lại trang không khôi phục liên kết đã bỏ khỏi thanh địa chỉ.');
      return;
    }
    setGrant(tokens[0]);
  }, []);

  useEffect(() => {
    // Replace the router's initial location too, so it cannot restore the stripped fragment.
    // The navigation carries no grant or other secret in its URL or state.
    if (location.hash) {
      void navigate(location.pathname + location.search, { replace: true, state: null });
    }
  }, [location.hash, location.pathname, location.search, navigate]);

  useEffect(() => {
    mounted.current = true;
    const previousTitle = document.title;
    document.title = 'Cập nhật thông tin đăng nhập ACB';
    return () => {
      mounted.current = false;
      saveController.current?.abort();
      document.title = previousTitle;
    };
  }, []);

  useEffect(() => {
    if (!grant) return;
    const controller = new AbortController();
    let active = true;
    setPhase('checking');
    setMessage('');
    void credentialRequest<CredentialGrantView>('/connection/credentials/grant', { grant }, controller)
      .then((result) => {
        if (!active) return;
        if (!Number.isSafeInteger(result.revision) || result.revision < 1 ||
          !Number.isFinite(Date.parse(result.expiresAt)) ||
          typeof result.accountMasked !== 'string' || typeof result.usernameMasked !== 'string' ||
          typeof result.canSave !== 'boolean' || typeof result.blockedReason !== 'string') {
          throw new Error('INVALID_GRANT_RESPONSE');
        }
        setView(result);
        setPhase('ready');
      })
      .catch((error: unknown) => {
        if (!active) return;
        setMessage(errorMessage(error));
        if (error instanceof ApiError && error.code === 'CSRF_TOKEN_INVALID') invalidateCsrfToken();
        if (error instanceof ApiError && (
          error.status === 410 ||
          ((error.status === 401 || error.status === 403) && error.code !== 'CSRF_TOKEN_INVALID') ||
          error.code === 'CREDENTIAL_GRANT_EXPIRED' || error.code === 'CREDENTIALS_REVISION_CONFLICT'
        )) {
          setGrant('');
          setView(null);
          setUsername('');
          setPassword('');
          setConfirmation('');
          setShowPasswords(false);
          setPhase('closed');
        } else {
          setPhase('validation-error');
        }
      });
    return () => {
      active = false;
      controller.abort();
    };
  }, [grant, validationRun]);

  useEffect(() => {
    if (!view || phase === 'saving' || phase === 'saved' || phase === 'unknown' || phase === 'closed') return;
    const expire = () => {
      setGrant('');
      setView(null);
      setUsername('');
      setPassword('');
      setConfirmation('');
      setShowPasswords(false);
      setPhase('closed');
      setMessage('Liên kết đã hết hạn. Hãy xin liên kết mới qua Đổi thông tin đăng nhập trong Telegram.');
    };
    const remaining = Date.parse(view.expiresAt) - Date.now();
    if (remaining <= 0) {
      expire();
      return;
    }
    const timer = setTimeout(expire, remaining);
    return () => clearTimeout(timer);
  }, [view, phase]);

  const handleSave = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (saveInFlight.current || phase !== 'ready' || !grant || !view?.canSave) return;
    if (Date.parse(view.expiresAt) <= Date.now()) {
      setGrant('');
      setPhase('closed');
      setUsername('');
      setPassword('');
      setConfirmation('');
      setShowPasswords(false);
      setMessage('Liên kết đã hết hạn. Hãy xin liên kết mới trong Telegram.');
      return;
    }
    const trimmedUsername = username.trim();
    const bytes = new TextEncoder();
    if (!trimmedUsername || !password || /[\r\n\0]/.test(username + password + confirmation) ||
      bytes.encode(trimmedUsername).length > 256 || bytes.encode(password).length > 1024) {
      setMessage('Tên đăng nhập cần 1–256 byte, mật khẩu cần 1–1024 byte; không dùng ký tự xuống dòng hoặc NUL. Khoảng trắng trong mật khẩu được giữ nguyên.');
      return;
    }
    if (password !== confirmation) {
      setMessage('Mật khẩu xác nhận chưa khớp. Kiểm tra cả khoảng trắng ở đầu và cuối.');
      return;
    }
    saveInFlight.current = true;
    const controller = new AbortController();
    saveController.current = controller;
    setPhase('saving');
    setShowPasswords(false);
    setMessage('');
    try {
      const result = await credentialRequest<CredentialSaveResult>('/connection/credentials', {
        grant,
        expectedRevision: view.revision,
        username: trimmedUsername,
        password,
        passwordConfirmation: confirmation,
      }, controller);
      if (!mounted.current) return;
      if (result.saved !== true || result.requiresLogin !== true || result.revision !== view.revision + 1) {
        throw new Error('INVALID_SAVE_RESPONSE');
      }
      setUsername('');
      setPassword('');
      setConfirmation('');
      setShowPasswords(false);
      setGrant('');
      setView(null);
      setPhase('saved');
    } catch (error: unknown) {
      if (!mounted.current) return;
      setPassword('');
      setConfirmation('');
      setShowPasswords(false);
      if (!(error instanceof ApiError) || (error.status >= 500 &&
        error.code !== 'CREDENTIALS_UNAVAILABLE' && error.code !== 'MUTATION_GATE_LOCKED')) {
        setUsername('');
        setGrant('');
        setView(null);
        setPhase('unknown');
        setMessage('Chưa xác định được việc lưu đã hoàn tất hay chưa. Trang sẽ không gửi lại yêu cầu. Hãy xem trạng thái/thông báo trong Telegram; nếu vẫn cần thay đổi, xin liên kết mới thay vì tải lại hoặc gửi lại từ trang này.');
      } else {
        setMessage(errorMessage(error));
        if (error.code === 'CSRF_TOKEN_INVALID') invalidateCsrfToken();
        if (error.status === 410 ||
          ((error.status === 401 || error.status === 403) && error.code !== 'CSRF_TOKEN_INVALID') ||
          error.code === 'CREDENTIAL_GRANT_EXPIRED' || error.code === 'CREDENTIALS_REVISION_CONFLICT') {
          setUsername('');
          setGrant('');
          setView(null);
          setPhase('closed');
        } else {
          setView({ ...view, canSave: false, blockedReason: error.code ?? '' });
          setPhase('ready');
        }
      }
    } finally {
      saveInFlight.current = false;
      saveController.current = null;
    }
  };

  const formEnabled = phase === 'ready' && view?.canSave === true;
  const expiresAt = view ? new Date(view.expiresAt) : null;
  const busyMessage = view?.blockedReason === 'ACB_SESSION_BUSY'
    ? errorMessage(new ApiError('', 409, 'ACB_SESSION_BUSY'))
    : 'Hiện chưa thể lưu. Hãy xem trạng thái trong Telegram, chờ thao tác hoặc bảo trì hoàn tất rồi kiểm tra lại. Trang này không tự đăng xuất hay hủy lượt đăng nhập.';

  return (
    <main lang="vi" className="min-h-screen bg-stone-50 px-4 py-8 sm:py-12 text-stone-900">
      <div className="mx-auto max-w-lg space-y-5">
        <header>
          <p className="mb-2 text-sm font-semibold text-emerald-700">ACB · Liên kết một lần từ Telegram</p>
          <h1 className="text-2xl sm:text-3xl font-bold tracking-tight">Cập nhật thông tin đăng nhập ACB</h1>
          <p className="mt-3 text-sm leading-6 text-stone-600">
            Chỉ thay đổi tên đăng nhập và mật khẩu lưu trong hệ thống trên VPS, không đổi mật khẩu tại ngân hàng và không chuyển số tài khoản theo dõi.
          </p>
        </header>

        <section className="rounded-2xl border border-stone-200 bg-white p-5 sm:p-6 shadow-2xs" aria-busy={phase === 'checking' || phase === 'saving'}>
          {phase === 'checking' && <p role="status" className="text-sm">Đang kiểm tra liên kết và quyền lưu…</p>}
          {phase === 'saving' && <p role="status" className="mb-4 text-sm">Đang lưu. Không tải lại trang hoặc gửi yêu cầu lần nữa…</p>}
          {message && <p role="alert" className="mb-4 rounded-xl border border-amber-200 bg-amber-50 p-3 text-sm leading-6 text-amber-950">{message}</p>}

          {phase === 'saved' && (
            <div role="status" className="space-y-3 text-sm leading-6">
              <h2 className="text-lg font-bold text-emerald-800">Đã lưu thông tin đăng nhập</h2>
              <p>Hệ thống chưa đăng nhập ACB. Tên đăng nhập và mật khẩu đã được xóa khỏi biểu mẫu này.</p>
              <p>Quay lại Telegram, xem thông báo và bấm Đăng nhập khi bạn muốn bắt đầu. Bạn có thể đóng trang này.</p>
            </div>
          )}

          {view && (phase === 'ready' || phase === 'saving') && (
            <>
              <dl className="mb-5 space-y-2 rounded-xl bg-stone-50 p-4 text-sm">
                <div className="flex flex-wrap justify-between gap-2">
                  <dt className="text-stone-600">Số tài khoản theo dõi</dt>
                  <dd className="break-all font-mono font-semibold">{view.accountMasked}</dd>
                </div>
                <div className="flex flex-wrap justify-between gap-2">
                  <dt className="text-stone-600">Tên đăng nhập đã lưu (che)</dt>
                  <dd className="break-all font-mono">{view.usernameMasked}</dd>
                </div>
                <div className="flex flex-wrap justify-between gap-2">
                  <dt className="text-stone-600">Liên kết hết hạn lúc</dt>
                  <dd><time dateTime={view.expiresAt}>{expiresAt?.toLocaleString('vi-VN')}</time></dd>
                </div>
              </dl>

              {!view.canSave && <p role="status" className="mb-4 text-sm leading-6 text-amber-900">{busyMessage}</p>}

              <form onSubmit={handleSave} autoComplete="off" className="space-y-4" aria-label="Thông tin đăng nhập mới">
                <div>
                  <label htmlFor="acb-credential-username" className="mb-1.5 block text-sm font-semibold">Tên đăng nhập</label>
                  <input id="acb-credential-username" name="username" type="text" value={username}
                    onChange={(event) => setUsername(event.target.value)} required maxLength={256}
                    autoComplete="off" autoCapitalize="none" spellCheck={false} disabled={!formEnabled}
                    aria-describedby="acb-credential-input-help" className={inputClass} />
                </div>
                <div>
                  <label htmlFor="acb-credential-password" className="mb-1.5 block text-sm font-semibold">Mật khẩu mới lưu trong hệ thống</label>
                  <input id="acb-credential-password" name="password" type={showPasswords ? 'text' : 'password'}
                    value={password} onChange={(event) => setPassword(event.target.value)} required maxLength={1024}
                    autoComplete="new-password" autoCapitalize="none" spellCheck={false} disabled={!formEnabled}
                    aria-describedby="acb-credential-input-help" className={inputClass} />
                </div>
                <div>
                  <label htmlFor="acb-credential-confirmation" className="mb-1.5 block text-sm font-semibold">Nhập lại mật khẩu</label>
                  <input id="acb-credential-confirmation" name="passwordConfirmation" type={showPasswords ? 'text' : 'password'}
                    value={confirmation} onChange={(event) => setConfirmation(event.target.value)} required maxLength={1024}
                    autoComplete="new-password" autoCapitalize="none" spellCheck={false} disabled={!formEnabled}
                    aria-describedby="acb-credential-input-help" className={inputClass} />
                </div>
                <p id="acb-credential-input-help" className="text-xs leading-5 text-stone-600">
                  Nhập đầy đủ thông tin mới; trang không điền thông tin bí mật cũ. Mật khẩu giữ nguyên mọi khoảng trắng, kể cả ở đầu và cuối.
                </p>
                <button type="button" onClick={() => setShowPasswords((shown) => !shown)}
                  aria-pressed={showPasswords} aria-controls="acb-credential-password acb-credential-confirmation"
                  disabled={!formEnabled} className={buttonClass}>
                  {showPasswords ? 'Ẩn mật khẩu' : 'Hiện mật khẩu'}
                </button>
                <button type="submit" disabled={!formEnabled}
                  className="min-h-12 w-full rounded-xl bg-emerald-700 px-4 py-3 text-base font-semibold text-white hover:bg-emerald-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-emerald-500 focus-visible:ring-offset-2 disabled:opacity-50 disabled:cursor-not-allowed">
                  {phase === 'saving' ? 'Đang lưu…' : 'Lưu thông tin đăng nhập'}
                </button>
              </form>
            </>
          )}

          {grant && (phase === 'validation-error' || (phase === 'ready' && !view?.canSave)) && (
            <button type="button" onClick={() => setValidationRun((run) => run + 1)} className={`${buttonClass} mt-4 w-full`}>
              Kiểm tra lại quyền lưu
            </button>
          )}
        </section>

        <p className="text-xs leading-5 text-stone-500">
          Liên kết chỉ được giữ trong bộ nhớ của trang và đã bỏ khỏi thanh địa chỉ. Không chia sẻ liên kết hay mật khẩu. Đóng hoặc tải lại trang sẽ cần xin liên kết mới trong Telegram.
        </p>
      </div>
    </main>
  );
};
