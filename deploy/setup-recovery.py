#!/usr/bin/env python3
"""Interactive setup for an already deployed, verified ACB release. No bank logout."""
import argparse
import contextlib
import fcntl
import getpass
import json
import os
import pwd
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
    def __init__(self, root, runner=None, ask=input, secret=getpass.getpass, telegram=Telegram):
        self.root = Path(root).resolve()
        self.runner = runner or self.run
        self.ask, self.secret, self.telegram = ask, secret, telegram
        self.env = self.root / "deploy/.env.production"
        self.secret_dir = self.root / "deploy/secrets"
        self.secrets_seen = []

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
                     "simple-lib.sh", "deploy.sh", "compose.prod.yaml", "compose.auth-recovery-ai.yaml"):
            if not (self.release / name).is_file():
                raise SetupError("Release thiếu " + name + "; chờ pipeline deploy bản mới.")
        self.checked(["sha256sum", "-c", "SHA256SUMS"], "Checksum release", cwd=self.release)
        self.checked(["docker", "info"], "Docker daemon")
        self.original, self.values = read_env(self.env)
        if self.values.get("AUTH_RECOVERY_ENABLED", "false") == "true":
            self.checked(["docker", "exec", "acb-recovery-controller", "/recovery-controller", "--readiness-check"],
                         "Recovery đã cấu hình; readiness")
            print("Recovery đã bật. Không thay secrets/config, không poll cạnh tranh. Dùng /acb_status trên Telegram.")
            return False
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
        schema = self.checked(["docker", "run", "--rm", "--network", "none", "--read-only", "--user", "1000:1000",
                               "-v", "bank-event-gateway_gateway_data:/data:ro", runtime["DBTOOL_IMAGE_REF"],
                               "-path", "/data/gateway.db", "-readonly", "-schema-version"], "Schema recovery")
        try:
            if json.loads(schema).get("version", 0) < 12:
                raise SetupError("Cần migration v12 qua pipeline deploy trước setup.")
        except (ValueError, TypeError):
            raise SetupError("Không đọc được schema report.") from None
        return True

    def configure(self):
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
            value = self.secret_file(name, label, pattern)
            del value
        changes = {
            "AUTH_RECOVERY_ENABLED": "true", "AI_CAPTCHA_ENABLED": "false",
            "TELEGRAM_CHAT_ID": user, "TELEGRAM_USER_ID": user,
            "AUTH_RECOVERY_CAPTCHA_TTL_SECONDS": "180", "AUTH_RECOVERY_OTP_TTL_SECONDS": "120",
            "TELEGRAM_BOT_TOKEN_FILE": "/run/secrets/telegram_bot_token",
            "ACB_USERNAME_FILE": "/run/secrets/acb_username", "ACB_PASSWORD_FILE": "/run/secrets/acb_password",
            "ACB_ACCOUNT_FILE": "/run/secrets/acb_account",
        }
        print("Human CAPTCHA hoạt động không cần AI. AI chỉ được bật nếu bạn đã kiểm tra logging/privacy của 9router và upstream.")
        if self.ask("Bật AI CAPTCHA ngay? [y/N]: ").strip().lower() in ("y", "yes"):
            base = self.ask("9router base URL [/v1; HTTPS hoặc hostname Docker private]: ").strip()
            model = self.ask("Exact vision model ID: ").strip()
            self.secret_file("ninerouter_api_key", "9router API key")
            changes.update(AI_CAPTCHA_ENABLED="true", NINEROUTER_BASE_URL=base,
                           NINEROUTER_CAPTCHA_MODEL=model, NINEROUTER_API_KEY_FILE="/run/secrets/ninerouter_api_key")
        print("Adapter ACB hiện có mới được kiểm chứng fixture. Bật sẽ cho phép tự login khi durable AUTH_REQUIRED;")
        print("không ép logout phiên đang khỏe. Nếu DOM/OTP ACB không hỗ trợ, bot dừng và hướng dẫn manual.")
        if self.ask("Bạn là chủ tài khoản và cho phép thử recovery có kiểm soát? Gõ BAT để kích hoạt: ").strip() != "BAT":
            raise SetupError("Chưa kích hoạt; secret files đã provision được giữ để chạy lại.")
        self.candidate = update_env(self.original, changes,
                                    removals=["APP_MASTER_KEY", "ACB_USERNAME", "ACB_PASSWORD",
                                              "ACB_ACCOUNT", "TELEGRAM_BOT_TOKEN", "NINEROUTER_API_KEY"])
        # Candidate config is used only by one-shot preflight, never runtime polling.
        stage = Path(tempfile.mkdtemp(prefix=".recovery-preflight-", dir=self.root))
        try:
            os.chmod(stage, 0o755)
            (stage / "deploy").mkdir(mode=0o755, exist_ok=True)
            candidate_env = stage / "deploy/.env.production"
            atomic_private(candidate_env, self.candidate)
            runtime_text, _ = read_env(self.release / "runtime.env")
            candidate_runtime = stage / "runtime.env"
            atomic_private(candidate_runtime, update_env(runtime_text, {
                "ENV_FILE": str(candidate_env), "SECRETS_DIR": str(self.secret_dir)}))
            self.compose(stage, candidate_runtime, "Compose config", "config", "--quiet")
            preflight_name = f"acb-recovery-preflight-{secrets.token_hex(4)}"
            self.compose(stage, candidate_runtime, "Preflight check-config",
                         "run", "-T", "--rm", "--no-deps", "--name", preflight_name,
                         "recovery-controller", "--check-config")
        finally:
            shutil.rmtree(stage)
        # Recheck original bytes; never overwrite another actor's config.
        if self.env.read_text(encoding="utf-8") != self.original:
            raise SetupError("Cấu hình đã thay đổi trong lúc setup; chưa kích hoạt.")
        atomic_private(self.env, self.candidate)

    def deploy_reconcile(self):
        # Preserve deployment UID and its Docker group; root is only for provisioning.
        owner = pwd.getpwuid(1000).pw_name
        self.checked(["runuser", "-u", owner, "--", "env", "DEPLOY_PATH=" + str(self.root),
                      "bash", str(self.release / "deploy.sh"), "--reconcile", self.sha],
                     "Recovery reconcile qua deploy admission")

    def activate(self):
        try:
            self.deploy_reconcile()
            self.checked(["docker", "exec", "acb-recovery-controller", "/recovery-controller", "--readiness-check"],
                         "Readiness sau kích hoạt")
        except (SetupError, subprocess.SubprocessError):
            # Don't revert a concurrently changed config, and don't blindly stop an active login.
            with file_lock(self.root / ".deploy.lock"):
                _, current = read_env(self.root / "state.env")
                if current.get("RELEASE_SHA") != self.sha:
                    raise SetupError("Deploy khác đã đổi release; không khôi phục cấu hình của release mới. Chạy lại setup để kiểm tra readiness.") from None
                if self.env.read_text(encoding="utf-8") != self.candidate:
                    raise SetupError("Cấu hình đã đổi đồng thời; giữ nguyên thay đổi đó, không rollback hoặc dừng controller.") from None
                atomic_private(self.env, self.original)
            # Existing admission path handles disable; it must wait if login is active.
            try:
                self.deploy_reconcile()
            except (SetupError, subprocess.SubprocessError):
                raise SetupError("Kích hoạt thất bại; cấu hình cũ đã khôi phục nếu không bị đổi đồng thời. Reconcile chưa hoàn tất; không force-cancel login.") from None
            raise SetupError("Kích hoạt thất bại; cấu hình cũ đã khôi phục và reconcile hoàn tất.") from None
        print("PASS: preflight + private operator reply + controller readiness. Recovery đã bật.")
        print("Từ giờ chỉ reply CAPTCHA/OTP khi bot yêu cầu; /acb_status, /acb_pause, /acb_resume dùng trên Telegram.")
        print("ACB live/catch-up chỉ được nghiệm thu sau lần recovery thật đầu tiên; readiness không phải bank success.")

    def execute(self):
        with file_lock(self.root / ".recovery-setup.lock"):
            with file_lock(self.root / ".deploy.lock"):
                if not self.prepare():
                    return
                self.configure()
            self.activate()


def main():
    parser = argparse.ArgumentParser(description="Setup recovery ACB trên release đã deploy: secrets nhập ẩn, private Telegram check, preflight, kích hoạt qua deploy admission.")
    parser.add_argument("--deploy-path", default=os.environ.get("DEPLOY_PATH", "/opt/bank-event-gateway"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise SetupError("Chạy bằng sudo bash setup-recovery.sh; cần tạo secret owner 1000:1000.")
    if not sys.stdin.isatty():
        raise SetupError("Cần terminal tương tác; không pipe token/password qua command line.")
    for tool in ("docker", "bash", "sha256sum", "python3", "runuser"):
        if not shutil.which(tool):
            raise SetupError("Thiếu dependency trên VPS: " + tool)
    Setup(args.deploy_path).execute()


if __name__ == "__main__":
    try:
        main()
    except (SetupError, OSError, EOFError, KeyboardInterrupt, subprocess.SubprocessError) as error:
        message = str(error) if isinstance(error, SetupError) else "Setup bị ngắt hoặc prerequisite không khả dụng; không in lỗi chứa secret."
        print("SETUP_FAILED: " + message, file=sys.stderr)
        sys.exit(1)
