"""Behavior tests use isolated deployment files and explicit upstream injection."""
import contextlib
import importlib.util
import io
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
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
        self.release = self.root / "releases" / ("a" * 40)
        self.release.mkdir(parents=True)
        (self.root / "state.env").write_text("RELEASE_SHA=" + "a" * 40 + "\n")
        (self.release / "runtime.env").write_text("ENV_FILE=/old/env\nSECRETS_DIR=/old/secrets\nWORKER_IMAGE_REF=registry/worker@sha256:abc\n")
        self.answers = iter(["123456", "", "BAT"])
        self.secret_answers = iter(["123:synthetic-token", "synthetic-user", " synthetic password ", "001234567"])
        self.preflight_fail = False
        self.concurrent_edit = False
        self.deploy_fail = False
        self.deploy_attempts = 0
        self.checked_candidate = False

        class Bot:
            def __init__(self, token):
                pass

            def verify_operator(self, user_id):
                return 4

        self.driver = setup.Setup(self.root, runner=self.runner,
                                  ask=lambda _: next(self.answers), secret=lambda _: next(self.secret_answers), telegram=Bot)
        self.driver.release, self.driver.sha = self.release, "a" * 40
        self.driver.original, self.driver.values = setup.read_env(self.env)
        # Tests run on any CI UID; only UID ownership syscall is substituted.
        self.chown = patch.object(setup.os, "fchown")
        self.chown.start()
        self.addCleanup(self.chown.stop)

    def runner(self, args, **kwargs):
        code = 0
        if args[0] == "bash":
            runtime = Path(args[6])
            _, runtime_values = setup.read_env(runtime)
            candidate_text, candidate = setup.read_env(Path(runtime_values["ENV_FILE"]))
            self.assertEqual(candidate["AUTH_RECOVERY_ENABLED"], "true")
            self.assertEqual(candidate["TELEGRAM_CHAT_ID"], "123456")
            self.assertEqual(self.env.read_text(), self.original)
            self.checked_candidate = True
            if "--check-config" in args:
                code = int(self.preflight_fail)
                if self.concurrent_edit:
                    self.env.write_text(self.original + "OTHER_NEW=concurrent\n")
        elif args[0] == "runuser":
            self.deploy_attempts += 1
            if self.deploy_fail and self.deploy_attempts == 1:
                code = 1
        return subprocess.CompletedProcess(args, code, stdout="", stderr="synthetic-token must never escape")

    def configure(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.driver.configure()
        self.assertNotIn("synthetic-token", output.getvalue())
        self.assertNotIn("synthetic password", output.getvalue())
        return output.getvalue()

    def test_human_setup_preserves_password_and_unrelated_config(self):
        self.configure()
        self.assertTrue(self.checked_candidate)
        text, values = setup.read_env(self.env)
        self.assertEqual(values["AUTH_RECOVERY_ENABLED"], "true")
        self.assertEqual(values["AI_CAPTCHA_ENABLED"], "false")
        self.assertIn("OTHER='value with spaces'\n", text)
        self.assertEqual((self.root / "deploy/secrets/acb_password").read_text(), " synthetic password \n")
        self.assertEqual(stat.S_IMODE((self.root / "deploy/secrets/telegram_bot_token").stat().st_mode), 0o600)
        self.assertFalse(list(self.root.glob(".recovery-preflight-*")))

    def test_preflight_failure_never_activates_or_changes_environment(self):
        self.preflight_fail = True
        with self.assertRaises(setup.SetupError) as caught:
            self.configure()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.deploy_attempts, 0)
        self.assertNotIn("synthetic-token", str(caught.exception))
        self.assertFalse(list(self.root.glob(".recovery-preflight-*")))

    def test_concurrent_config_change_is_not_overwritten(self):
        self.concurrent_edit = True
        with self.assertRaises(setup.SetupError):
            self.configure()
        self.assertEqual(self.env.read_text(), self.original + "OTHER_NEW=concurrent\n")

    def test_existing_credentials_are_reused_without_asking(self):
        existing_value = " " + setup.secrets.token_hex(16) + " "
        for name, value in {"telegram_bot_token": "123:synthetic-token", "acb_username": "existing",
                            "acb_password": existing_value, "acb_account": "123456"}.items():
            path = self.root / "deploy/secrets" / name
            path.write_text(value + "\n")
            path.chmod(0o600)
        with patch.object(setup, "secure_existing"):
            self.driver.secret = lambda _: self.fail("existing credential prompted again")
            self.configure()
        self.assertEqual((self.root / "deploy/secrets/acb_password").read_text(), existing_value + "\n")

    def test_failed_activation_restores_config_without_removing_credentials(self):
        self.configure()
        self.deploy_fail = True
        with self.assertRaises(setup.SetupError):
            self.driver.activate()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertTrue((self.root / "deploy/secrets/acb_password").is_file())
        self.assertEqual(self.deploy_attempts, 2)

    def test_new_release_configuration_is_never_rolled_back(self):
        self.configure()
        self.deploy_fail = True
        (self.root / "state.env").write_text("RELEASE_SHA=" + "b" * 40 + "\n")
        with self.assertRaises(setup.SetupError):
            self.driver.activate()
        self.assertEqual(self.env.read_text(), self.driver.candidate)
        self.assertEqual(self.deploy_attempts, 1)

    def test_failed_activation_does_not_reconcile_concurrent_config(self):
        self.configure()
        self.deploy_fail = True
        external = self.driver.candidate + "EXTERNAL_SETTING=keep\n"
        self.env.write_text(external)
        with self.assertRaises(setup.SetupError):
            self.driver.activate()
        self.assertEqual(self.env.read_text(), external)
        self.assertEqual(self.deploy_attempts, 1)

    def test_declined_activation_leaves_flags_off(self):
        self.answers = iter(["123456", "", "NO"])
        with self.assertRaises(setup.SetupError):
            self.configure()
        self.assertEqual(self.env.read_text(), self.original)
        self.assertEqual(self.deploy_attempts, 0)

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
