"""Behavior tests use isolated deployment files and explicit upstream injection."""
import contextlib
import importlib.util
import io
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
import threading
import urllib.error
import urllib.request
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("setup_recovery", Path(__file__).parents[1] / "setup-recovery.py")
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


class SetupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "deploy/secrets").mkdir(parents=True)
        self.env = self.root / "deploy/.env.production"
        self.original = "PUBLIC_ORIGIN=https://bank.example\nAUTH_RECOVERY_ENABLED=false\nAI_CAPTCHA_ENABLED=false\n# preserved\nOTHER='value with spaces'\n"
        self.env.write_text(self.original)
        self.env.chmod(0o600)
        self.release = self.root / "releases" / ("a" * 40)
        self.release.mkdir(parents=True)
        (self.root / "state.env").write_text("RELEASE_SHA=" + "a" * 40 + "\n")
        (self.release / "runtime.env").write_text(
            f"ENV_FILE={self.env}\nSECRETS_DIR={self.root / 'deploy/secrets'}\n"
            "WORKER_IMAGE_REF=registry/worker@sha256:abc\nDBTOOL_IMAGE_REF=registry/dbtool@sha256:abc\n")
        self.answers = iter(["123456", "", "BAT"])
        self.secret_answers = iter(["123:synthetic-token", "synthetic-user", " synthetic password ", "001234567"])
        self.preflight_fail = False
        self.failures = []
        self.concurrent_edit = False
        self.concurrent_key = False
        self.admission_fail = False
        self.deploy_fail = False
        self.deploy_attempts = 0
        self.checked_candidate = False
        self.admitted = False
        self.running = False
        self.bound_key = None
        self.requests = []
        self.runtime_requests = []
        self.bot_calls = []
        self.runtime_mutation = None
        self.import_fail = False
        self.import_calls = 0
        owner = self

        class Provider(BaseHTTPRequestHandler):
            def do_POST(self):
                authorization = self.headers.get("Authorization")
                owner.requests.append(authorization)
                if authorization not in {"Bearer old-key", "Bearer new-key", "Bearer fixed-key"}:
                    self.send_response(401)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"text":"AB12CD"}')

            def log_message(self, *args):
                pass

        self.provider = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
        self.thread = threading.Thread(target=self.provider.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.close_provider)

        class Bot:
            def __init__(self, token):
                owner.bot_calls.append("token")

            def verify_operator(self, user_id):
                owner.bot_calls.append("enrollment")
                return 4

        self.driver = setup.Setup(self.root, runner=self.runner,
                                  ask=lambda _: next(self.answers), secret=lambda _: next(self.secret_answers), telegram=Bot)
        self.driver.release, self.driver.sha = self.release, "a" * 40
        self.driver.original, self.driver.values = setup.read_env(self.env)
        self.key = self.root / "deploy/secrets/ninerouter_api_key"
        self.chown = patch.object(setup.os, "fchown")
        self.chown.start()
        self.addCleanup(self.chown.stop)
        # Ignore only CI UID; preserve regular-file, symlink and permission boundaries.
        def secure(path, mode):
            valid_type = path.is_dir() if mode == 0o700 else path.is_file()
            if path.is_symlink() or not valid_type or stat.S_IMODE(path.stat().st_mode) != mode:
                raise setup.SetupError("unsafe fixture file")
        self.secure = patch.object(setup, "secure_existing", side_effect=secure)
        self.secure.start()
        self.addCleanup(self.secure.stop)

    def close_provider(self):
        self.provider.shutdown()
        self.provider.server_close()
        self.thread.join()

    def request_provider(self, key):
        request = urllib.request.Request(f"http://127.0.0.1:{self.provider.server_port}/completion", data=b"synthetic",
                                         headers={"Authorization": "Bearer " + key.strip()})
        try:
            with urllib.request.urlopen(request, timeout=2) as response:
                self.assertEqual(json.loads(response.read()), {"text": "AB12CD"})
                return response.status
        except urllib.error.HTTPError as error:
            return error.code

    def runner(self, args, **kwargs):
        code, stdout, stderr = 0, "", ""
        if "-gate-acquire" in args:
            if self.admission_fail:
                return subprocess.CompletedProcess(args, 1, "", "ACTIVE_AUTH_ATTEMPT")
            self.assertFalse(self.admitted)
            self.admitted = True
            stdout = '{"leaseToken":"fixture-lease"}'
        elif "-gate-release" in args:
            self.assertTrue(self.admitted)
            self.admitted = False
        elif "-gate-renew" in args:
            self.assertTrue(self.admitted)
        elif args[0] == "bash" and "validate_recovery_controller_bundle" in args[2]:
            pass
        elif args[0] == "bash" and "import_recovery_credentials" in args[2]:
            self.assertFalse(self.admitted)
            self.assertFalse(self.running)
            self.import_calls += 1
            if self.import_fail:
                code, stderr = 1, "CREDENTIAL_IMPORT_FILE_INVALID"
        elif args[0] == "bash":
            runtime = Path(args[6])
            _, runtime_values = setup.read_env(runtime)
            _, candidate = setup.read_env(Path(runtime_values["ENV_FILE"]))
            if "--check-config" in args or "--check-ai" in args:
                self.assertEqual(self.env.read_text(), self.original)
                self.checked_candidate = True
                provider_status = 0
                if candidate.get("AI_CAPTCHA_ENABLED") == "true":
                    override_index = args.index("-f")
                    override = json.loads(Path(args[override_index + 1]).read_text())
                    candidate_key = Path(override["secrets"]["ninerouter_api_key"]["file"])
                    self.assertNotEqual(candidate_key, self.key)
                    self.assertEqual(self.key.read_text() if self.key.exists() else None, self.driver.old_ai_key)
                    provider_status = self.request_provider(candidate_key.read_text())
                failure = self.failures.pop(0) if self.failures else ("HTTP_AUTH" if provider_status == 401 else None)
                if failure:
                    code = 1
                    stderr = json.dumps({"reason": "AI_VISION_PREFLIGHT_UNAVAILABLE", "stage": "MODEL_DISCOVERY",
                                         "code": failure, "http_status": 401, "msg": "RAW_PROVIDER_SECRET"})
                elif self.preflight_fail:
                    code, stderr = 1, "unstructured token must never escape"
                if self.concurrent_edit:
                    self.env.write_text(self.original + "OTHER_NEW=concurrent\n")
                if self.concurrent_key:
                    self.key.write_text("concurrent-key\n")
            elif "up" in args:
                self.assertFalse(self.admitted)
                self.assertIn("--force-recreate", args)
                self.deploy_attempts += 1
                self.running = True
                self.bound_key = self.key.read_text() if candidate.get("AI_CAPTCHA_ENABLED") == "true" else None
                if self.bound_key:
                    self.assertEqual(self.request_provider(self.bound_key), 200)
                    self.runtime_requests.append(self.requests[-1])
                if self.runtime_mutation:
                    self.runtime_mutation()
                if self.deploy_fail and self.deploy_attempts == 1:
                    code = 1
        elif args[:2] == ["docker", "inspect"]:
            stdout = "true" if self.running else "false"
        elif args[:2] == ["docker", "stop"]:
            self.assertTrue(self.admitted)
            self.running = False
        elif "-schema-compat" in args:
            stdout = '{"compatible":true,"schemaVersion":13}'
        return subprocess.CompletedProcess(args, code, stdout=stdout, stderr=stderr)

    def configure(self, activate=False):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.driver.configure()
            if activate:
                self.driver.activate()
        self.assertNotIn("synthetic-token", output.getvalue())
        self.assertNotIn("synthetic password", output.getvalue())
        self.assertNotIn("RAW_PROVIDER_SECRET", output.getvalue())
        return output.getvalue()

    def enable_existing_ai(self, enabled=True):
        self.original = ("PUBLIC_ORIGIN=https://bank.example\nAUTH_RECOVERY_ENABLED=true\n"
                         "AI_CAPTCHA_ENABLED=" + str(enabled).lower() + "\nTELEGRAM_CHAT_ID=123456\nTELEGRAM_USER_ID=123456\n"
                         "NINEROUTER_BASE_URL=https://provider.example/v1\nNINEROUTER_CAPTCHA_MODEL=exact-vision\n"
                         "NINEROUTER_API_KEY_FILE=/run/secrets/ninerouter_api_key\n")
        self.env.write_text(self.original)
        self.key.write_text("old-key\n")
        self.key.chmod(0o600)
        self.driver.original, self.driver.values = setup.read_env(self.env)
        self.driver.configure_ai = True
        self.running = True
        self.answers = iter(["", "", "y"])
        self.secret_answers = iter(["new-key"])

    def test_human_setup_preserves_password_and_unrelated_config(self):
        self.configure(activate=True)
        self.assertTrue(self.checked_candidate)
        text, values = setup.read_env(self.env)
        self.assertEqual(values["AUTH_RECOVERY_ENABLED"], "true")
        self.assertEqual(values["AI_CAPTCHA_ENABLED"], "false")
        self.assertIn("OTHER='value with spaces'\n", text)
        self.assertEqual((self.root / "deploy/secrets/acb_password").read_text(), " synthetic password \n")
        self.assertFalse(list(self.root.glob(".recovery-preflight-*")))

    def test_preflight_failure_never_activates_or_changes_environment(self):
        self.preflight_fail = True
        with self.assertRaises(setup.SetupError) as caught:
            self.configure()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.deploy_attempts, 0)
        self.assertNotIn("token", str(caught.exception))
        self.assertFalse(list(self.root.glob(".recovery-preflight-*")))

    def test_initial_ai_failure_defaults_to_stop_without_provisioning_active_key(self):
        self.answers = iter(["123456", "y", "https://provider.example/v1", "vision", "BAT", ""])
        self.secret_answers = iter(["123:synthetic-token", "user", "password", "001234", "new-key"])
        self.failures = ["HTTP_AUTH"]
        with self.assertRaises(setup.SetupError):
            self.configure()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertFalse(self.key.exists())
        self.assertEqual(self.requests, ["Bearer new-key"])
        self.assertFalse(self.running)

    def test_key_only_rotation_uses_candidate_then_recreated_runtime(self):
        self.enable_existing_ai()
        self.configure()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.key.read_text(), "old-key\n")
        self.assertEqual(self.bot_calls, [])
        self.assertEqual(self.requests, ["Bearer new-key"])
        self.driver.activate()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.runtime_requests, ["Bearer new-key"])
        self.assertEqual(self.key.read_text(), "new-key\n")

    def test_failed_rotation_restores_key_env_and_old_runtime_request(self):
        self.enable_existing_ai()
        self.configure()
        self.deploy_fail = True
        with self.assertRaises(setup.SetupError):
            self.driver.activate()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.key.read_text(), "old-key\n")
        self.assertEqual(self.runtime_requests, ["Bearer new-key", "Bearer old-key"])
        self.assertFalse(self.admitted)

    def test_environment_replace_then_fsync_failure_rolls_back_both_promotions(self):
        self.enable_existing_ai()
        self.answers = iter(["", "new-vision", "y"])
        self.configure()
        original_write = setup.atomic_private
        failed = False
        def fail_after_replace(path, text):
            nonlocal failed
            original_write(path, text)
            if path == self.env and text == self.driver.candidate and not failed:
                failed = True
                raise OSError("fixture directory fsync failure")
        with patch.object(setup, "atomic_private", side_effect=fail_after_replace), self.assertRaises(setup.SetupError):
            self.driver.activate()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.key.read_text(), "old-key\n")
        self.assertEqual(self.runtime_requests, ["Bearer old-key"])


    def test_ai_disabled_existing_runtime_can_be_configured(self):
        self.enable_existing_ai(enabled=False)
        for name in ("app_master_key", "worker_internal_token", "auth_browser_internal_token"):
            path = self.root / "deploy/secrets" / name
            path.write_text("fixture")
            path.chmod(0o600)
        for name in ("images.env", "SHA256SUMS", "setup-recovery.sh", "setup-recovery.py", "simple-lib.sh",
                     "deploy.sh", "healthcheck.sh", "compose.prod.yaml", "compose.auth-recovery-ai.yaml"):
            (self.release / name).write_text("fixture")
        self.assertTrue(self.driver.prepare())
        self.configure(activate=True)
        self.assertEqual(setup.read_env(self.env)[1]["AI_CAPTCHA_ENABLED"], "true")
        self.assertEqual(self.bot_calls, [])

    def test_structured_failure_guidance_fix_then_retry_candidate(self):
        self.enable_existing_ai()
        self.answers = iter(["", "", "y", "1", "", "new-vision", "y"])
        self.secret_answers = iter(["bad-key", "fixed-key"])
        output = self.configure(activate=True)
        self.assertIn("HTTP_AUTH", output)
        self.assertIn("entitlement", output)
        self.assertEqual(self.requests, ["Bearer bad-key", "Bearer fixed-key", "Bearer fixed-key"])
        self.assertEqual(setup.read_env(self.env)[1]["NINEROUTER_CAPTCHA_MODEL"], "new-vision")

    def test_only_explicit_degraded_choice_disables_ai_and_preserves_active_key(self):
        self.enable_existing_ai()
        self.answers = iter(["", "", "y", "2"])
        self.failures = ["MODEL_NOT_FOUND"]
        self.configure(activate=True)
        self.assertEqual(setup.read_env(self.env)[1]["AI_CAPTCHA_ENABLED"], "false")
        self.assertEqual(self.key.read_text(), "old-key\n")
        self.assertEqual(self.runtime_requests, [])

    def test_existing_key_keep_choice_never_asks_hidden_input(self):
        self.enable_existing_ai()
        self.answers = iter(["", "", ""])
        self.driver.secret = lambda _: self.fail("unexpected key replacement")
        self.configure(activate=True)
        self.assertEqual(self.requests, ["Bearer old-key", "Bearer old-key"])

    def test_admission_rejection_keeps_active_key_and_runtime_untouched(self):
        self.enable_existing_ai()
        self.configure()
        self.admission_fail = True
        with self.assertRaises(setup.SetupError):
            self.driver.activate()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.key.read_text(), "old-key\n")
        self.assertTrue(self.running)
        self.assertEqual(self.deploy_attempts, 0)

    def test_concurrent_config_or_key_change_is_not_overwritten(self):
        for target in ("env", "key"):
            with self.subTest(target=target):
                self.enable_existing_ai()
                self.concurrent_edit, self.concurrent_key = target == "env", target == "key"
                with self.assertRaises(setup.SetupError):
                    self.configure()
                self.assertEqual(self.deploy_attempts, 0)
                if target == "env":
                    self.assertEqual(self.env.read_text(), self.original + "OTHER_NEW=concurrent\n")
                else:
                    self.assertEqual(self.key.read_text(), "concurrent-key\n")

    def test_concurrent_release_after_preflight_is_not_promoted(self):
        self.enable_existing_ai()
        self.configure()
        (self.root / "state.env").write_text("RELEASE_SHA=" + "b" * 40 + "\n")
        with self.assertRaises(setup.SetupError):
            self.driver.activate()
        self.assertEqual(self.key.read_text(), "old-key\n")
        self.assertEqual(self.deploy_attempts, 0)

    def test_concurrent_key_on_activation_failure_is_not_rolled_back(self):
        self.enable_existing_ai()
        self.configure()
        self.deploy_fail = True
        self.runtime_mutation = lambda: self.key.write_text("external-key\n")
        with self.assertRaises(setup.SetupError):
            self.driver.activate()
        self.assertEqual(self.key.read_text(), "external-key\n")
        self.assertEqual(self.deploy_attempts, 1)

    def test_failed_initial_activation_restores_flags_and_removes_new_ai_key(self):
        self.answers = iter(["123456", "y", "https://provider.example/v1", "vision", "BAT"])
        self.secret_answers = iter(["123:synthetic-token", "user", "password", "001234", "new-key"])
        self.configure()
        self.deploy_fail = True
        with self.assertRaises(setup.SetupError):
            self.driver.activate()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertFalse(self.key.exists())
        self.assertFalse(self.running)

    def test_declined_activation_leaves_flags_off(self):
        self.answers = iter(["123456", "", "NO"])
        with self.assertRaises(setup.SetupError):
            self.configure()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.deploy_attempts, 0)

    def test_existing_legacy_credentials_are_not_overwritten(self):
        password = " existing-password "
        for name, value in {"telegram_bot_token": "123:synthetic-token", "acb_username": "existing-user",
                            "acb_password": password, "acb_account": "001234"}.items():
            path = self.root / "deploy/secrets" / name
            path.write_text(value + "\n")
            path.chmod(0o600)
        self.driver.secret = lambda _: self.fail("existing secret unexpectedly requested")
        self.configure(activate=True)
        self.assertEqual((self.root / "deploy/secrets/acb_password").read_text(), password + "\n")

    def test_explicit_import_does_not_enroll_or_preflight_and_restarts_after_gate_release(self):
        self.enable_existing_ai(enabled=False)
        self.driver.configure_ai = False
        self.driver.import_credentials = True
        self.driver.import_existing()
        self.assertEqual(self.import_calls, 1)
        self.assertTrue(self.running)
        self.assertFalse(self.admitted)
        self.assertEqual(self.bot_calls, [])
        self.assertEqual(self.requests, [])
        self.assertFalse(self.checked_candidate)
        self.assertEqual(self.env.read_text(), self.original)

    def test_explicit_import_preparation_does_not_take_enabled_readiness_shortcut(self):
        self.enable_existing_ai(enabled=False)
        self.driver.configure_ai = False
        self.driver.import_credentials = True
        for name in ("app_master_key", "worker_internal_token", "auth_browser_internal_token"):
            path = self.root / "deploy/secrets" / name
            path.write_text("fixture")
            path.chmod(0o600)
        for name in ("images.env", "SHA256SUMS", "setup-recovery.sh", "setup-recovery.py", "simple-lib.sh",
                     "deploy.sh", "healthcheck.sh", "compose.prod.yaml", "compose.auth-recovery-ai.yaml"):
            (self.release / name).write_text("fixture")
        self.assertTrue(self.driver.prepare())
        self.assertTrue(self.running)
        self.assertEqual(self.bot_calls, [])
        self.assertEqual(self.import_calls, 0)

    def test_explicit_import_cannot_interrupt_active_otp(self):
        self.enable_existing_ai(enabled=False)
        self.driver.configure_ai = False
        self.driver.import_credentials = True
        self.admission_fail = True
        with self.assertRaises(setup.SetupError):
            self.driver.import_existing()
        self.assertTrue(self.running)
        self.assertEqual(self.import_calls, 0)
        self.assertEqual(self.deploy_attempts, 0)

    def test_failed_explicit_import_leaves_bot_available_without_changing_env(self):
        self.enable_existing_ai(enabled=False)
        self.driver.configure_ai = False
        self.driver.import_credentials = True
        self.import_fail = True
        with self.assertRaises(setup.SetupError):
            self.driver.import_existing()
        self.assertTrue(self.running)
        self.assertFalse(self.admitted)
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.bot_calls, [])

    def test_symlink_secret_cannot_be_overwritten(self):
        target = self.root / "unrelated"
        target.write_text("untouched")
        (self.root / "deploy/secrets/telegram_bot_token").symlink_to(target)
        with self.assertRaises(setup.SetupError):
            self.configure()
        self.assertEqual(target.read_text(), "untouched")

    def test_unsafe_env_value_and_duplicate_keys_are_rejected(self):
        with self.assertRaises(setup.SetupError):
            setup.update_env("AUTH_RECOVERY_ENABLED=false\nAUTH_RECOVERY_ENABLED=false\n", {"AUTH_RECOVERY_ENABLED": "true"})
        for value in ("token\nOTHER=injected", "$(shell)", "value # comment", ""):
            with self.subTest(value=value), self.assertRaises(setup.SetupError):
                setup.update_env(self.original, {"NINEROUTER_CAPTCHA_MODEL": value})

    def test_diagnostic_parser_rejects_unstructured_and_untrusted_fields(self):
        for record in ("reason=AI_VISION_PREFLIGHT_UNAVAILABLE code=HTTP_AUTH",
                       {"reason": "AI_VISION_PREFLIGHT_UNAVAILABLE", "stage": "RAW_SECRET", "code": "HTTP_AUTH"},
                       {"reason": "AI_VISION_PREFLIGHT_UNAVAILABLE", "stage": "MODEL_DISCOVERY", "code": "HTTP_AUTH", "http_status": "secret"}):
            self.assertIsNone(setup.ai_diagnostic(record if isinstance(record, str) else json.dumps(record)))
        diagnostic = setup.ai_diagnostic(json.dumps({"reason": "AI_VISION_PREFLIGHT_UNAVAILABLE", "stage": "SYNTHETIC_OCR",
                                                     "code": "OCR_MISMATCH", "http_status": 200, "body": "RAW_SECRET"}))
        self.assertEqual(str(diagnostic), "AI_VISION_PREFLIGHT_UNAVAILABLE stage=SYNTHETIC_OCR code=OCR_MISMATCH http_status=200")

class TelegramTests(unittest.TestCase):
    def valid(self):
        return {"message_id": 2, "chat": {"id": 123, "type": "private"}, "from": {"id": 123},
                "text": " AB12CD ", "reply_to_message": {"message_id": 1}}

    def test_private_reply_fences(self):
        self.assertTrue(setup.valid_setup_reply(self.valid(), 123, 1, "AB12CD"))
        for field, value in (("from", {"id": 999}), ("from", {"id": 123, "is_bot": True}),
                             ("chat", {"id": 123, "type": "group"}), ("reply_to_message", {"message_id": 99}),
                             ("forward_origin", {}), ("via_bot", {}), ("photo", []), ("text", "wrong")):
            incoming = self.valid()
            incoming[field] = value
            with self.subTest(field=field, value=value):
                self.assertFalse(setup.valid_setup_reply(incoming, 123, 1, "AB12CD"))

    def test_webhook_conflict_does_not_poll_or_delete_webhook(self):
        bot = setup.Telegram("123:synthetic")
        calls = []
        def api(method, payload):
            calls.append(method)
            return {"url": "https://example.invalid/webhook"} if method == "getWebhookInfo" else {}
        bot.call = api
        with self.assertRaises(setup.SetupError):
            bot.verify_operator(123)
        self.assertEqual(calls, ["getMe", "getWebhookInfo"])

    def test_operator_handshake_ignores_forwarded_reply_then_cleans_synthetic(self):
        bot = setup.Telegram("123:synthetic")
        deleted = []
        def api(method, payload):
            if method == "getWebhookInfo": return {"url": ""}
            if method == "getChat": return {"id": 123, "type": "private"}
            if method == "sendMessage": return {"message_id": 1}
            if method == "getUpdates":
                bad = self.valid()
                bad["forward_origin"] = {}
                return [{"update_id": 1, "message": bad}, {"update_id": 2, "message": self.valid()}]
            if method == "deleteMessage": deleted.append(payload["message_id"])
            return {}
        bot.call = api
        with patch.object(setup.secrets, "token_hex", return_value="ab12cd"), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(bot.verify_operator(123), 3)
        self.assertEqual(deleted, [2, 1])

    def test_network_failure_does_not_expose_token_url(self):
        bot = setup.Telegram("123:synthetic")
        with patch.object(bot.opener, "open", side_effect=setup.urllib.error.URLError("https://api.telegram.org/bot123:synthetic/getMe")):
            with self.assertRaises(setup.SetupError) as caught:
                bot.call("getMe", {})
        self.assertNotIn("123:synthetic", str(caught.exception))


if __name__ == "__main__":
    unittest.main()
