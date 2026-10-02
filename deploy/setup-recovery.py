#!/usr/bin/env python3
"""Interactive setup for an already deployed, verified ACB release. No bank logout."""
import argparse
import contextlib
import fcntl
import getpass
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


class SetupError(Exception):
    pass


AI_STAGES = {"MODEL_DISCOVERY", "SYNTHETIC_OCR", "CAPTCHA_OCR"}
AI_CODES = {"DNS", "TLS", "TIMEOUT", "CANCELLED", "NETWORK", "HTTP_AUTH", "HTTP_NOT_FOUND",
            "HTTP_RATE_LIMIT", "HTTP_UPSTREAM", "MODEL_NOT_FOUND", "MODEL_COMBO_UNSUPPORTED",
            "RESPONSE_TOO_LARGE", "RESPONSE_SCHEMA", "REFUSAL", "OCR_MISMATCH"}


class AIPreflightError(SetupError):
    def __init__(self, stage, code, http_status):
        self.stage, self.code, self.http_status = stage, code, http_status
        super().__init__(f"AI_VISION_PREFLIGHT_UNAVAILABLE stage={stage} code={code} http_status={http_status}")


def ai_diagnostic(stderr):
    # Only JSON fields with the controller's finite diagnostic contract are trusted.
    for line in (stderr or "").splitlines():
        try:
            record = json.loads(line)
        except (ValueError, TypeError):
            continue
        if not isinstance(record, dict) or record.get("reason") != "AI_VISION_PREFLIGHT_UNAVAILABLE":
            continue
        stage, code, status = record.get("stage"), record.get("code"), record.get("http_status", 0)
        if (isinstance(stage, str) and stage in AI_STAGES and isinstance(code, str) and code in AI_CODES
                and type(status) is int and (status == 0 or 100 <= status <= 599)):
            return AIPreflightError(stage, code, status)
    return None


def read_env(path):
    text = path.read_text(encoding="utf-8")
    values = {}
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        key, sep, value = line.partition("=")
        if sep:
            if key in values:
                raise SetupError("Cấu hình có key trùng; không tự ghi đè: " + key)
            values[key] = value.strip().strip("\"'")
    return text, values


def update_env(text, changes, removals=None):
    # Only setup-owned keys are replaced; unrelated deployment config stays verbatim.
    for key, value in changes.items():
        if not re.fullmatch(r"[A-Z_]+", key) or not value or any(c in value for c in "\r\n\x00$'\"# "):
            raise SetupError("Giá trị cấu hình không an toàn: " + key)
    removals_set = set(removals or ())
    output = []
    remaining = dict(changes)
    seen = set()
    for line in text.splitlines():
        key, sep, _ = line.partition("=")
        if sep and key in removals_set:
            continue
        if sep and key in changes:
            if key in seen:
                raise SetupError("Cấu hình có key trùng: " + key)
            seen.add(key)
            output.append(key + "=" + remaining.pop(key))
        else:
            output.append(line)
    output.extend(key + "=" + value for key, value in remaining.items())
    return "\n".join(output) + "\n"


def atomic_private(path, text):
    if path.is_symlink():
        raise SetupError("Không ghi đè đường dẫn symlink: " + path.name)
    fd, temp = tempfile.mkstemp(prefix=".recovery-", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        os.fchown(fd, 1000, 1000)
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as stream:
            stream.write(text)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, path)
        directory = os.open(path.parent, os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def secure_existing(path, mode):
    if path.is_symlink():
        raise SetupError("Không dùng symlink cho cấu hình/secret: " + path.name)
    st = path.stat()
    expected_type = stat.S_ISDIR if mode == 0o700 else stat.S_ISREG
    if not expected_type(st.st_mode) or (stat.S_IMODE(st.st_mode), st.st_uid, st.st_gid) != (mode, 1000, 1000):
        raise SetupError(f"{path.name} phải có mode {mode:04o}, owner 1000:1000; không tự sửa file đang vận hành.")


@contextlib.contextmanager
def file_lock(path):
    fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        os.fchown(fd, 1000, 1000)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise SetupError("Deploy/setup khác đang chạy; thử lại khi hoàn tất.") from None
        yield
    finally:
        os.close(fd)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Telegram:
    def __init__(self, token):
        if not re.fullmatch(r"[0-9]+:[A-Za-z0-9_-]+", token):
            raise SetupError("Token Telegram không đúng định dạng.")
        self.token = token
        self.opener = urllib.request.build_opener(NoRedirect())

    def call(self, method, payload):
        data = json.dumps(payload).encode()
        request = urllib.request.Request("https://api.telegram.org/bot" + self.token + "/" + method,
                                         data=data, headers={"Content-Type": "application/json"})
        try:
            with self.opener.open(request, timeout=40) as response:
                raw = response.read((1 << 20) + 1)
                if len(raw) > 1 << 20:
                    raise SetupError("TELEGRAM_RESPONSE_LIMIT")
                result = json.loads(raw)
            if not result.get("ok"):
                raise SetupError("TELEGRAM_REQUEST_REJECTED")
            return result["result"]
        except (urllib.error.URLError, ValueError, KeyError, TimeoutError, OSError):
            # urllib exceptions contain token-bearing URLs: never print them.
            raise SetupError("TELEGRAM_UNAVAILABLE: kiểm tra token, /start và kết nối mạng.") from None

    def verify_operator(self, user_id):
        self.call("getMe", {})
        if self.call("getWebhookInfo", {}).get("url"):
            raise SetupError("TELEGRAM_WEBHOOK_CONFLICT: cần bot riêng, không tự xóa webhook.")
        chat = self.call("getChat", {"chat_id": user_id})
        if chat.get("id") != user_id or chat.get("type") != "private":
            raise SetupError("TELEGRAM_PRIVATE_CHAT_REQUIRED")
        code = secrets.token_hex(3).upper()
        message = self.call("sendMessage", {
            "chat_id": user_id,
            "text": "Thiết lập ACB. Đây là mã thử, KHÔNG phải OTP ngân hàng. Trả lời trực tiếp tin này bằng: " + code,
            "protect_content": True,
            "reply_markup": {"force_reply": True, "selective": True},
        })
        prompt_id = message["message_id"]
        print("Đã gửi mã thử vào Telegram. Reply trực tiếp tin của bot để xác nhận quyền; tối đa 120 giây.", flush=True)
        offset = 0
        deadline = time.monotonic() + 120
        try:
            while time.monotonic() < deadline:
                updates = self.call("getUpdates", {"offset": offset, "timeout": 20, "limit": 100,
                                                  "allowed_updates": ["message", "callback_query"]})
                for update in updates:
                    offset = max(offset, int(update["update_id"]) + 1)
                    incoming = update.get("message", {})
                    if valid_setup_reply(incoming, user_id, prompt_id, code):
                        try:
                            self.call("deleteMessage", {"chat_id": user_id, "message_id": incoming["message_id"]})
                        except SetupError:
                            pass
                        return offset
            raise SetupError("TELEGRAM_OPERATOR_CONFIRMATION_EXPIRED: chưa nhận đúng reply của operator.")
        finally:
            try:
                self.call("deleteMessage", {"chat_id": user_id, "message_id": prompt_id})
            except SetupError:
                pass


def valid_setup_reply(message, user_id, prompt_id, code):
    chat, sender = message.get("chat", {}), message.get("from", {})
    unsafe = ("forward_origin", "forward_from", "forward_from_chat", "forward_date", "via_bot", "sender_chat",
              "photo", "document", "contact", "video", "voice", "audio", "sticker")
    return (chat.get("type") == "private" and chat.get("id") == user_id and sender.get("id") == user_id
            and not sender.get("is_bot", False) and not any(key in message for key in unsafe)
            and message.get("reply_to_message", {}).get("message_id") == prompt_id
            and isinstance(message.get("text"), str) and message["text"].strip() == code)


class Setup:
    def __init__(self, root, runner=None, ask=input, secret=getpass.getpass, telegram=Telegram, configure_ai=False, import_credentials=False):
        self.root = Path(root).resolve()
        self.runner = runner or self.run
        self.ask, self.secret, self.telegram = ask, secret, telegram
        self.env = self.root / "deploy/.env.production"
        self.secret_dir = self.root / "deploy/secrets"
        self.secrets_seen = []
        self.configure_ai = configure_ai
        self.import_credentials = import_credentials
        self.stage = None
        self.ai_key_path = self.secret_dir / "ninerouter_api_key"
        self.old_ai_key = None
        self.candidate_ai_key = None
        self.key_snapshot_taken = False

    def sanitize(self, text):
        if not text:
            return ""
        for s in sorted(set(self.secrets_seen), key=lambda x: len(x), reverse=True):
            if s and len(s) >= 4:
                text = text.replace(s, "[REDACTED]")
        text = re.sub(r"[0-9]+:[A-Za-z0-9_-]{8,}", "[REDACTED_TELEGRAM_TOKEN]", text)
        text = re.sub(r"(token|password|secret|key)[=:\s]+[^\s]+", r"\1=[REDACTED]", text, flags=re.IGNORECASE)
        return text

    @staticmethod
    def run(args, **kwargs):
        return subprocess.run(args, capture_output=True, text=True, timeout=900, **kwargs)

    def checked(self, args, label, **kwargs):
        result = self.runner(args, **kwargs)
        if result.returncode:
            detail = self.sanitize((result.stderr or result.stdout or "").strip())
            if detail:
                summary = " ".join(detail.split())[-400:]
                raise SetupError(f"{label} thất bại (mã {result.returncode}): {summary}")
            raise SetupError(f"{label} thất bại (mã {result.returncode}).")
        return result.stdout.strip()

    def secret_file(self, name, label, pattern=None):
        path = self.secret_dir / name
        if path.exists() or path.is_symlink():
            secure_existing(path, 0o600)
            value = path.read_text(encoding="utf-8").removesuffix("\n").removesuffix("\r")
        else:
            value = self.secret(label + " (ẩn khi nhập): ")
            if not value or any(c in value for c in "\r\n\x00"):
                raise SetupError("Secret phải là một dòng không rỗng: " + name)
            if pattern and not re.fullmatch(pattern, value):
                raise SetupError("Secret không đúng định dạng: " + name)
            atomic_private(path, value + "\n")
        if not value or (pattern and not re.fullmatch(pattern, value)):
            raise SetupError("Secret không hợp lệ: " + name)
        self.secrets_seen.append(value)
        if ":" in value:
            self.secrets_seen.append(value.split(":", 1)[1])
        if value.strip() != value:
            self.secrets_seen.append(value.strip())
        return value

    def compose(self, stage_root, runtime, label, *args):
        return self.checked(["bash", "-c",
                             'source "$1/simple-lib.sh"; DEPLOY_PATH="$2"; compose_release "$1" "$3" "${@:4}"',
                             "setup", str(self.release), str(stage_root), str(runtime), *args],
                            label)


    def prepare(self):
        secure_existing(self.env, 0o600)
        secure_existing(self.secret_dir, 0o700)
        _, state = read_env(self.root / "state.env")
        self.sha = state.get("RELEASE_SHA", "")
        if not re.fullmatch(r"[a-f0-9]{40}", self.sha):
            raise SetupError("Chưa có release đã commit hợp lệ trên VPS.")
        self.release = self.root / "releases" / self.sha
        for name in ("runtime.env", "images.env", "SHA256SUMS", "setup-recovery.sh", "setup-recovery.py",
                     "simple-lib.sh", "deploy.sh", "healthcheck.sh", "compose.prod.yaml", "compose.auth-recovery-ai.yaml"):
            if not (self.release / name).is_file():
                raise SetupError("Release thiếu " + name + "; chờ pipeline deploy bản mới.")
        self.checked(["sha256sum", "-c", "SHA256SUMS"], "Checksum release", cwd=self.release)
        self.checked(["docker", "info"], "Docker daemon")
        self.original, self.values = read_env(self.env)
        enabled = self.values.get("AUTH_RECOVERY_ENABLED", "false") == "true"
        if enabled and not self.configure_ai and not self.import_credentials:
            self.checked(["docker", "exec", "acb-recovery-controller", "/recovery-controller", "--readiness-check"],
                         "Recovery đã cấu hình; readiness")
            print("Recovery đã bật. Dùng /menu; sửa AI bằng --configure-ai, migrate bằng --import-credentials.")
            return False
        if self.configure_ai and not enabled:
            raise SetupError("--configure-ai yêu cầu recovery đã bật; chạy setup ban đầu để cấu hình Telegram.")
        if self.import_credentials and not enabled:
            raise SetupError("--import-credentials yêu cầu recovery đã bật; chạy setup ban đầu nếu chưa cấu hình Telegram.")
        if not enabled:
            inspect = self.runner(["docker", "inspect", "-f", "{{.State.Running}}", "acb-recovery-controller"])
            if inspect.returncode == 0 and inspect.stdout.strip() == "true":
                raise SetupError("Controller đang chạy dù flag tắt; cần reconcile deploy trước, không dừng OTP bằng setup.")
        for name in ("app_master_key", "worker_internal_token", "auth_browser_internal_token"):
            if not (self.secret_dir / name).is_file():
                raise SetupError("Thiếu secret vận hành " + name + "; không tự sinh/đổi token hoặc master key.")
            secure_existing(self.secret_dir / name, 0o600)
        # Existing service image and migrations must already be installed by CI.
        _, runtime = read_env(self.release / "runtime.env")
        self.checked(["docker", "image", "inspect", runtime["WORKER_IMAGE_REF"]], "Worker image của release")
        self.checked(["bash", "-c", 'source "$1/simple-lib.sh"; validate_recovery_controller_bundle "$1"',
                      "setup", str(self.release)], "Admission controller Telegram v13")
        schema = self.checked(["docker", "run", "--rm", "--network", "none", "--read-only", "--user", "1000:1000",
                               "-v", "bank-event-gateway_gateway_data:/data:ro", runtime["DBTOOL_IMAGE_REF"],
                               "-path", "/data/gateway.db", "-readonly", "-schema-compat", "-min-version", "13"], "Schema recovery")
        try:
            report = json.loads(schema)
            if report.get("compatible") is not True or report.get("schemaVersion", 0) < 13:
                raise SetupError("Cần migration v13/checksum hợp lệ qua pipeline deploy trước setup.")
        except (ValueError, TypeError):
            raise SetupError("Không đọc được schema report.") from None
        return True

    def ai_settings(self, changes):
        base = self.ask("9router base URL [/v1] [" + changes.get("NINEROUTER_BASE_URL", self.values.get("NINEROUTER_BASE_URL", "")) + "]: ").strip()
        model = self.ask("Exact vision model ID [" + changes.get("NINEROUTER_CAPTCHA_MODEL", self.values.get("NINEROUTER_CAPTCHA_MODEL", "")) + "]: ").strip()
        base = base or changes.get("NINEROUTER_BASE_URL", self.values.get("NINEROUTER_BASE_URL", ""))
        model = model or changes.get("NINEROUTER_CAPTCHA_MODEL", self.values.get("NINEROUTER_CAPTCHA_MODEL", ""))
        if not self.key_snapshot_taken:
            if self.ai_key_path.exists() or self.ai_key_path.is_symlink():
                secure_existing(self.ai_key_path, 0o600)
                self.old_ai_key = self.ai_key_path.read_text(encoding="utf-8")
            self.key_snapshot_taken = True
        existing = self.candidate_ai_key if self.candidate_ai_key is not None else self.old_ai_key
        replace = existing is None or self.ask("Đã có AI key. Thay bằng key mới? [y/N]: ").strip().lower() in ("y", "yes")
        value = self.secret("9router API key mới (ẩn khi nhập): ") if replace else existing.removesuffix("\n").removesuffix("\r")
        if not value or any(c in value for c in "\r\n\x00"):
            raise SetupError("AI key phải là một dòng không rỗng.")
        self.secrets_seen.append(value)
        self.candidate_ai_key = value + "\n"
        changes.update(AI_CAPTCHA_ENABLED="true", NINEROUTER_BASE_URL=base,
                       NINEROUTER_CAPTCHA_MODEL=model, NINEROUTER_API_KEY_FILE="/run/secrets/ninerouter_api_key")

    def configure(self):
        changes = {}
        if self.configure_ai:
            print("Chỉ cấu hình AI: không enrollment/poll Telegram, không đọc mật khẩu hoặc đăng nhập ACB.")
            self.ai_settings(changes)
        else:
            print("Thiết lập recovery ACB: không logout ngân hàng, không đổi master key/internal tokens.")
            print("Bot chat không E2EE; chỉ dùng OTP đăng nhập. Mở bot riêng và gửi /start trước.")
            token = self.secret_file("telegram_bot_token", "Bot token", r"[0-9]+:[A-Za-z0-9_-]+")
            configured_id = self.values.get("TELEGRAM_USER_ID", "")
            user = self.ask("Telegram user ID của bạn" + (" [" + configured_id + "]" if configured_id else "") + ": ").strip() or configured_id
            if not re.fullmatch(r"[1-9][0-9]{0,18}", user) or int(user) > (1 << 63) - 1:
                raise SetupError("Cần numeric user ID dương của private operator.")
            self.telegram(token).verify_operator(int(user))
            del token
            for name, label, pattern in (("acb_username", "Username ACB", None),
                                         ("acb_password", "Password ACB", None),
                                         ("acb_account", "Số tài khoản ACB đầy đủ", r"[0-9]+")):
                self.secret_file(name, label, pattern)
            changes.update(AUTH_RECOVERY_ENABLED="true", AI_CAPTCHA_ENABLED="false",
                           TELEGRAM_CHAT_ID=user, TELEGRAM_USER_ID=user,
                           AUTH_RECOVERY_CAPTCHA_TTL_SECONDS="180", AUTH_RECOVERY_OTP_TTL_SECONDS="120",
                           TELEGRAM_BOT_TOKEN_FILE="/run/secrets/telegram_bot_token")
            print("AI chỉ bật sau khi bạn kiểm tra logging/privacy của provider. Human CAPTCHA là dự phòng.")
            if self.ask("Bật AI CAPTCHA ngay? [y/N]: ").strip().lower() in ("y", "yes"):
                self.ai_settings(changes)
            print("Bật bot không đăng nhập ACB. Mỗi lần đăng nhập chỉ bắt đầu sau nút Đăng nhập được xác thực.")
            if self.ask("Bạn là chủ tài khoản? Gõ BAT để kích hoạt: ").strip() != "BAT":
                raise SetupError("Chưa kích hoạt; secret files đã provision được giữ để chạy lại.")
        while True:
            self.candidate = update_env(self.original, changes,
                                       removals=["APP_MASTER_KEY", "ACB_USERNAME", "ACB_PASSWORD", "ACB_ACCOUNT",
                                                 "ACB_USERNAME_FILE", "ACB_PASSWORD_FILE", "ACB_ACCOUNT_FILE",
                                                 "TELEGRAM_BOT_TOKEN", "NINEROUTER_API_KEY"])
            try:
                self.preflight()
                break
            except AIPreflightError as error:
                self.explain_ai(error)
                choice = self.ask("(1) Sửa URL/model/key và thử synthetic lại; (2) AI tắt, CAPTCHA thủ công; (3) Dừng [3]: ").strip()
                if choice == "1":
                    self.ai_settings(changes)
                elif choice == "2":
                    changes["AI_CAPTCHA_ENABLED"] = "false"
                else:
                    raise SetupError("AI chưa PASS; đã dừng, không thay cấu hình/key đang hoạt động.") from None
        self.recheck(self.original, self.old_ai_key)

    @staticmethod
    def explain_ai(error):
        print(str(error))
        if error.code in {"DNS", "NETWORK", "TLS", "TIMEOUT", "CANCELLED"}:
            print("Kiểm tra egress/DNS/chứng chỉ từ network controller; không tắt xác minh TLS.")
        elif error.code == "HTTP_AUTH":
            print("Sửa secret API key/quyền entitlement tại provider; không liên quan mật khẩu ACB.")
        elif error.code == "HTTP_NOT_FOUND":
            print("Kiểm tra base URL đúng /v1.")
        elif error.code in {"MODEL_NOT_FOUND", "MODEL_COMBO_UNSUPPORTED"}:
            print("Chọn exact model vision thực có; không dùng combo hoặc tự chọn model khác.")
        elif error.code == "HTTP_RATE_LIMIT":
            print("Chờ theo retry_after/quota của upstream, không retry mù.")
        else:
            print("Kiểm tra contract multimodal/JSON của model tại provider; AI chỉ bật khi synthetic AB12CD PASS.")
        print("AI preflight không kiểm tra mật khẩu ACB. Sau import, đổi thông tin qua /menu → Đổi thông tin đăng nhập → HTTPS.")

    def preflight(self):
        self.cleanup()
        self.stage = Path(tempfile.mkdtemp(prefix=".recovery-preflight-", dir=self.root))
        try:
            os.chmod(self.stage, 0o755)
            (self.stage / "deploy").mkdir(mode=0o755)
            candidate_env = self.stage / "deploy/.env.production"
            atomic_private(candidate_env, self.candidate)
            runtime_text, _ = read_env(self.release / "runtime.env")
            candidate_runtime = self.stage / "runtime.env"
            atomic_private(candidate_runtime, update_env(runtime_text, {
                "ENV_FILE": str(candidate_env), "SECRETS_DIR": str(self.secret_dir)}))
            override_args = []
            _, candidate = read_env(candidate_env)
            if candidate.get("AI_CAPTCHA_ENABLED") == "true":
                key = self.stage / "ninerouter_api_key"
                atomic_private(key, self.candidate_ai_key)
                override = self.stage / "candidate-ai.json"
                atomic_private(override, json.dumps({"secrets": {"ninerouter_api_key": {"file": str(key)}}}))
                override_args = ["-f", str(override)]
            self.compose(self.stage, candidate_runtime, "Compose config", *override_args, "config", "--quiet")
            if self.configure_ai and candidate.get("AI_CAPTCHA_ENABLED") == "false":
                # Explicit degraded choice: no synthetic PASS claim and no Telegram polling/checks.
                return
            args = ["bash", "-c", 'source "$1/simple-lib.sh"; DEPLOY_PATH="$2"; compose_release "$1" "$3" "${@:4}"',
                    "setup", str(self.release), str(self.stage), str(candidate_runtime), *override_args,
                    "run", "-T", "--rm", "--no-deps", "--name", "acb-recovery-preflight-" + secrets.token_hex(4),
                    "recovery-controller", "--check-ai" if self.configure_ai else "--check-config"]
            result = self.runner(args)
            if result.returncode:
                diagnostic = ai_diagnostic(result.stderr)
                if diagnostic is not None:
                    raise diagnostic
                raise SetupError("Preflight thất bại; không có diagnostic AI có cấu trúc hợp lệ. Chưa kích hoạt.")
        finally:
            self.cleanup()

    def cleanup(self):
        if self.stage is not None:
            shutil.rmtree(self.stage)
            self.stage = None

    def recheck(self, expected_env, expected_key):
        _, state = read_env(self.root / "state.env")
        if state.get("RELEASE_SHA") != self.sha:
            raise SetupError("Release đã thay đổi; không ghi hoặc rollback cấu hình của release mới.")
        secure_existing(self.env, 0o600)
        if self.env.read_text(encoding="utf-8") != expected_env:
            raise SetupError("Cấu hình đã thay đổi đồng thời; không ghi đè hoặc rollback.")
        if self.key_snapshot_taken:
            actual = None
            if self.ai_key_path.exists() or self.ai_key_path.is_symlink():
                secure_existing(self.ai_key_path, 0o600)
                actual = self.ai_key_path.read_text(encoding="utf-8")
            if actual != expected_key:
                raise SetupError("AI key đã thay đổi đồng thời; không ghi đè hoặc rollback.")

    def dbtool(self, *args):
        _, runtime = read_env(self.release / "runtime.env")
        return self.checked(["bash", "-c", 'source "$1/simple-lib.sh"; DBTOOL_IMAGE_REF="$2"; dbtool rw "${@:3}"',
                             "setup", str(self.release), runtime["DBTOOL_IMAGE_REF"], *args], "Deployment admission")

    @contextlib.contextmanager
    def admission(self):
        owner = "recovery-setup-" + secrets.token_hex(8)
        try:
            lease = json.loads(self.dbtool("-gate-acquire", "-owner", owner, "-reason", "recovery-reconfigure",
                                          "-lease-duration", "15m"))["leaseToken"]
        except (ValueError, TypeError, KeyError):
            raise SetupError("Deployment admission không trả lease hợp lệ; không thay cấu hình.") from None
        if not isinstance(lease, str) or not lease:
            raise SetupError("Deployment admission thiếu lease; không thay cấu hình.")
        self.secrets_seen.append(lease)
        try:
            yield owner, lease
        finally:
            self.dbtool("-gate-release", "-owner", owner, "-lease-token", lease)

    def stop_admitted(self):
        inspect = self.runner(["docker", "inspect", "-f", "{{.State.Running}}", "acb-recovery-controller"])
        if inspect.returncode == 0 and inspect.stdout.strip() == "true":
            self.checked(["docker", "stop", "acb-recovery-controller"], "Dừng controller sau admission")

    def start_runtime(self, enabled):
        if enabled:
            self.checked(["bash", "-c",
                          'source "$1/simple-lib.sh"; DEPLOY_PATH="$2"; import_recovery_credentials "$1" "$3" "$4"',
                          "setup", str(self.release), str(self.root), str(self.release / "runtime.env"),
                          "true" if self.import_credentials else "false"], "Import thông tin ACB")
            # Atomic key replacement changes inode; even a key-only rotation must remount it.
            self.compose(self.root, self.release / "runtime.env", "Recovery force recreate",
                         "up", "-d", "--no-deps", "--force-recreate", "recovery-controller")
            _, runtime = read_env(self.release / "runtime.env")
            self.checked(["env", "EXPECTED_IMAGE_REF=" + runtime["WORKER_IMAGE_REF"], "bash",
                          str(self.release / "healthcheck.sh"), "container", "acb-recovery-controller", "120"],
                         "Image và readiness sau recreate")

    def activate(self):
        promote_key = self.candidate_ai_key is not None and "AI_CAPTCHA_ENABLED=true\n" in self.candidate
        expected_env, expected_key = self.original, self.old_ai_key
        stopped = False
        try:
            with file_lock(self.root / ".deploy.lock"):
                self.recheck(expected_env, expected_key)
                with self.admission() as (owner, lease):
                    self.recheck(expected_env, expected_key)
                    stopped = True
                    self.stop_admitted()
                    self.dbtool("-gate-renew", "-owner", owner, "-lease-token", lease, "-lease-duration", "15m")
                    if promote_key:
                        try:
                            atomic_private(self.ai_key_path, self.candidate_ai_key)
                        finally:
                            # os.replace may succeed even if the following directory fsync fails.
                            if not self.ai_key_path.is_symlink() and self.ai_key_path.exists():
                                if self.ai_key_path.read_text(encoding="utf-8") == self.candidate_ai_key:
                                    expected_key = self.candidate_ai_key
                    try:
                        atomic_private(self.env, self.candidate)
                    finally:
                        if not self.env.is_symlink() and self.env.read_text(encoding="utf-8") == self.candidate:
                            expected_env = self.candidate
                # Keep deployment lock through recreate/readiness; release mutation gate before runtime starts.
                self.start_runtime(True)
        except (SetupError, OSError, KeyboardInterrupt, subprocess.SubprocessError):
            if not stopped:
                raise
            try:
                with file_lock(self.root / ".deploy.lock"):
                    self.recheck(expected_env, expected_key)
                    with self.admission():
                        self.recheck(expected_env, expected_key)
                        self.stop_admitted()
                        if promote_key:
                            if self.old_ai_key is None:
                                self.ai_key_path.unlink(missing_ok=True)
                            else:
                                atomic_private(self.ai_key_path, self.old_ai_key)
                        atomic_private(self.env, self.original)
                    self.start_runtime(self.values.get("AUTH_RECOVERY_ENABLED", "false") == "true")
            except (SetupError, OSError, KeyboardInterrupt, subprocess.SubprocessError):
                raise SetupError("Kích hoạt thất bại; rollback chưa hoàn tất (admission/concurrency/runtime). Không force-cancel đăng nhập; kiểm tra release/readiness rồi reconcile.") from None
            raise SetupError("Kích hoạt thất bại; env và AI key cũ đã khôi phục, runtime cũ được admit/recreate nếu đã bật.") from None
        if self.configure_ai:
            print("PASS: synthetic preflight + controller readiness. AI đã cấu hình." if promote_key else
                  "PASS: preflight + controller readiness. AI tắt theo lựa chọn rõ ràng; CAPTCHA thủ công.")
        else:
            print("PASS: preflight + private operator reply + controller readiness. Recovery đã bật.")
        print("Dùng /menu trên Telegram. Chỉ nút Đăng nhập mới bắt đầu ACB; readiness không phải bank success.")

    def import_existing(self):
        # No enrollment/preflight or bank access. Admission refuses an active OTP
        # attempt before stop, then releases the gate before the importer writes.
        with file_lock(self.root / ".deploy.lock"):
            self.recheck(self.original, self.old_ai_key)
            with self.admission():
                self.recheck(self.original, self.old_ai_key)
                self.stop_admitted()
            try:
                self.start_runtime(True)
            except (SetupError, OSError, KeyboardInterrupt, subprocess.SubprocessError):
                # Keep the admitted controller available even if import is invalid.
                # No legacy credential fallback is mounted in this runtime.
                self.compose(self.root, self.release / "runtime.env", "Khởi động bot sau import thất bại",
                             "up", "-d", "--no-deps", "--force-recreate", "recovery-controller")
                raise
        print("Import hoàn tất (record hiện hữu không ghi đè). Chưa đăng nhập ACB; dùng /menu.")

    def execute(self):
        try:
            with file_lock(self.root / ".recovery-setup.lock"):
                with file_lock(self.root / ".deploy.lock"):
                    if not self.prepare():
                        return
                    if not self.import_credentials:
                        self.configure()
                if self.import_credentials:
                    self.import_existing()
                else:
                    self.activate()
        finally:
            self.cleanup()


def main():
    parser = argparse.ArgumentParser(description="Setup recovery ACB trên release đã deploy: secrets nhập ẩn, private Telegram check, preflight, kích hoạt qua deploy admission.")
    parser.add_argument("--deploy-path", default=os.environ.get("DEPLOY_PATH", "/opt/bank-event-gateway"))
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--configure-ai", action="store_true", help="Sửa AI/rotate key đã bật, không enrollment hoặc poll Telegram")
    modes.add_argument("--import-credentials", action="store_true", help="Import legacy files khi DB chưa cấu hình; không ghi đè hoặc đăng nhập")
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise SetupError("Chạy bằng sudo bash setup-recovery.sh; cần tạo secret owner 1000:1000.")
    if not sys.stdin.isatty():
        raise SetupError("Cần terminal tương tác; không pipe token/password qua command line.")
    for tool in ("docker", "bash", "sha256sum", "python3"):
        if not shutil.which(tool):
            raise SetupError("Thiếu dependency trên VPS: " + tool)
    Setup(args.deploy_path, configure_ai=args.configure_ai, import_credentials=args.import_credentials).execute()


if __name__ == "__main__":
    try:
        main()
    except (SetupError, OSError, EOFError, KeyboardInterrupt, subprocess.SubprocessError) as error:
        message = str(error) if isinstance(error, SetupError) else "Setup bị ngắt hoặc prerequisite không khả dụng; không in lỗi chứa secret."
        print("SETUP_FAILED: " + message, file=sys.stderr)
        sys.exit(1)
