"""Operator behavior against a loopback HTTP fixture, never real Telegram."""
import base64
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("sepay_telegram_ops", ROOT / "scripts/ops/sepay-telegram.py")
OPS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OPS)

TOKEN = "900001:owner_fixture_token_DO_NOT_LOG"
SECRET = base64.urlsafe_b64encode(bytes(range(32))).decode().rstrip("=")
ORIGIN = "https://cashier.example"
CALLBACK = ORIGIN + OPS.CALLBACK_PATH
NOW = 1800000000


class LocalTransport:
    """Rewrite only the wire destination, preserving the real API request."""
    def __init__(self, origin):
        self.origin = origin
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), OPS.NoRedirect())

    def open(self, request, timeout):
        if not request.full_url.startswith(OPS.API_ORIGIN + "/"):
            raise AssertionError("unexpected API destination")
        local = urllib.request.Request(
            self.origin + request.full_url[len(OPS.API_ORIGIN):], data=request.data,
            headers=dict(request.header_items()), method=request.get_method())
        return self.opener.open(local, timeout=timeout)


class TelegramOperatorTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.config_path = Path(self.directory.name) / "store.json"
        self.token_path = Path(self.directory.name) / "bot-token"
        self.config = {"mode": "observe", "botId": 900001, "webhookSecret": SECRET}
        self.write_config()
        self.token_path.write_text(TOKEN + "\n", encoding="utf-8")
        self.token_path.chmod(0o600)
        self.requests = []
        self.responses = {
            "getMe": {"ok": True, "result": {"id": 900001, "is_bot": True}},
            "getWebhookInfo": {"ok": True, "result": {"url": "", "pending_update_count": 0}},
            "setWebhook": {"ok": True, "result": True},
        }
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                raw = self.rfile.read(int(self.headers["Content-Length"]))
                fixture.requests.append({"path": self.path, "method": "POST",
                                         "type": self.headers["Content-Type"], "body": json.loads(raw)})
                method = self.path.rsplit("/", 1)[-1]
                response = fixture.responses.get(method, {"ok": False})
                if "redirect" in response:
                    self.send_response(302)
                    self.send_header("Location", response["redirect"])
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                if isinstance(response, tuple):
                    code, response = response
                else:
                    code = 200
                body = json.dumps(response).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            def do_GET(self):
                fixture.requests.append({"path": self.path, "method": "GET"})
                self.send_response(200)
                self.send_header("Content-Length", "0")
                self.end_headers()


            def log_message(self, format, *args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.transport = LocalTransport("http://127.0.0.1:" + str(self.server.server_port))

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.directory.cleanup()

    def write_config(self):
        self.config_path.write_text(json.dumps(self.config), encoding="utf-8")
        self.config_path.chmod(0o600)

    def invoke(self, command="register", origin=ORIGIN, extra=()):
        out, err = io.StringIO(), io.StringIO()
        argv = [command, "--config-file", str(self.config_path), "--token-file", str(self.token_path),
                "--public-origin", origin, *extra]
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            try:
                code = OPS.main(argv, api_factory=lambda token: OPS.TelegramAPI(token, self.transport), now=NOW)
            except SystemExit as error:
                code = error.code
        output, errors = out.getvalue(), err.getvalue()
        self.assertNotIn(TOKEN, output + errors)
        self.assertNotIn(SECRET, output + errors)
        return code, output, errors

    def methods(self):
        return [request["path"].rsplit("/", 1)[-1] for request in self.requests]

    def set_info(self, **values):
        self.responses["getWebhookInfo"]["result"] = {
            "url": CALLBACK, "pending_update_count": 0, **values}

    def test_register_verifies_identity_and_preserves_pending_updates(self):
        code, output, errors = self.invoke()
        self.assertEqual((code, errors), (0, ""))
        self.assertEqual(json.loads(output), {"url": CALLBACK, "registered": True})
        self.assertEqual(self.methods(), ["getMe", "getWebhookInfo", "setWebhook"])
        for request in self.requests:
            self.assertEqual(request["method"], "POST")
            self.assertEqual(request["type"], "application/json")
            self.assertTrue(request["path"].startswith("/bot" + TOKEN + "/"))
        self.assertEqual(self.requests[0]["body"], {})
        self.assertEqual(self.requests[1]["body"], {})
        self.assertEqual(self.requests[2]["body"], {
            "url": CALLBACK, "secret_token": SECRET,
            "allowed_updates": ["message", "edited_message"],
            "max_connections": 4, "drop_pending_updates": False})

    def test_register_accepts_same_url_and_active_mode(self):
        self.config["mode"] = "active"
        self.write_config()
        self.set_info(pending_update_count=12)
        self.assertEqual(self.invoke()[0], 0)
        self.assertEqual(self.requests[-1]["body"]["drop_pending_updates"], False)

    def test_register_refuses_to_take_over_existing_webhook(self):
        self.set_info(url="https://another.example/" + TOKEN + "?secret=" + SECRET)
        code, output, errors = self.invoke()
        self.assertEqual(code, 1)
        self.assertEqual(output, "")
        self.assertIn("refusing", errors)
        self.assertEqual(self.methods(), ["getMe", "getWebhookInfo"])

    def test_register_stops_before_webhook_lookup_on_identity_mismatch(self):
        for identity in ({"id": 900002, "is_bot": True}, {"id": 900001, "is_bot": False},
                         {"id": "900001", "is_bot": True}):
            with self.subTest(identity=identity):
                self.requests.clear()
                self.responses["getMe"]["result"] = identity
                code, _, errors = self.invoke()
                self.assertEqual(code, 1)
                self.assertIn("identity", errors)
                self.assertEqual(self.methods(), ["getMe"])

    def test_register_requires_positive_confirmation(self):
        self.responses["setWebhook"]["result"] = False
        code, output, errors = self.invoke()
        self.assertEqual(code, 1)
        self.assertEqual(output, "")
        self.assertIn("did not confirm", errors)

    def test_origin_rejections_never_contact_api(self):
        for origin in ("http://cashier.example", "https://cashier.example/", "https://cashier.example/path",
                       "https://user:password@cashier.example", "https://cashier.example?secret=" + SECRET,
                       "https://cashier.example?", "https://cashier.example#", "https://cashier.example#fragment",
                       "https://", "https://cashier.example:bad", "https://cashier.example:0",
                       "https://cashier.example:", "https://cashier.example\\path", "https://cashier.example\n"):
            with self.subTest(origin=origin):
                self.assertEqual(self.invoke(origin=origin)[0], 1)
                self.assertEqual(self.requests, [])

    def test_invalid_secret_inputs_never_contact_api(self):
        for field, value in (("mode", "disabled"), ("botId", True), ("botId", 0), ("botId", "900001"),
                             ("botId", 2**63), ("webhookSecret", "short"),
                             ("webhookSecret", SECRET + "="), ("webhookSecret", SECRET[:-1] + "9")):
            with self.subTest(field=field, value=value):
                self.config = {"mode": "observe", "botId": 900001, "webhookSecret": SECRET, field: value}
                self.write_config()
                self.assertEqual(self.invoke()[0], 1)
                self.assertEqual(self.requests, [])

    def test_invalid_json_duplicate_keys_and_token_are_not_echoed(self):
        for raw in ('{"mode": "observe", "botId":900001,"botId":900002}',
                    '[]', '{"webhookSecret":"' + SECRET + '"', '{}{}'):
            with self.subTest(raw=raw):
                self.config_path.write_text(raw, encoding="utf-8")
                self.assertEqual(self.invoke()[0], 1)
                self.assertEqual(self.requests, [])
        self.write_config()
        self.token_path.write_text(TOKEN + " trailing-secret", encoding="utf-8")
        code, _, errors = self.invoke()
        self.assertEqual(code, 1)
        self.assertNotIn("trailing-secret", errors)
        self.assertEqual(self.requests, [])

    @unittest.skipIf(os.name == "nt", "Windows uses file ACLs, not POSIX mode 0600")
    def test_both_secret_files_require_exact_private_permissions(self):
        for path in (self.config_path, self.token_path):
            for mode in (0o644, 0o640, 0o400, 0o700):
                with self.subTest(file=path.name, mode=mode):
                    path.chmod(mode)
                    try:
                        code, _, errors = self.invoke()
                        self.assertEqual(code, 1)
                        self.assertIn("0600", errors)
                        self.assertEqual(self.requests, [])
                    finally:
                        path.chmod(0o600)

    def test_api_failure_descriptions_and_http_error_bodies_stay_private(self):
        for response in ({"ok": False, "description": TOKEN + " " + SECRET},
                         (401, {"description": TOKEN + " " + SECRET}),
                         {"ok": True}, {"ok": True, "result": {"id": TOKEN}}):
            with self.subTest(response=response):
                self.requests.clear()
                self.responses["getMe"] = response
                code, output, errors = self.invoke()
                self.assertEqual(code, 1)
                self.assertEqual(output, "")
                self.assertTrue(errors.startswith("error: Telegram"))
                self.assertEqual(self.methods(), ["getMe"])

    def test_status_reports_safe_fields_without_registering_or_polling(self):
        self.set_info()
        code, output, errors = self.invoke("status")
        self.assertEqual((code, errors), (0, ""))
        self.assertEqual(json.loads(output), {
            "url": CALLBACK, "url_matches": True, "pending_update_count": 0,
            "last_error_at": None, "last_error_message": "", "recent_delivery_error": False})
        self.assertEqual(self.methods(), ["getWebhookInfo"])
    def test_redirects_cannot_forward_token_to_a_different_receiver(self):
        self.responses["getMe"] = {
            "redirect": self.transport.origin + "/bot" + TOKEN + "/stolen"}
        code, output, errors = self.invoke()
        self.assertEqual(code, 1)
        self.assertEqual(output, "")
        self.assertIn("request failed", errors)
        self.assertEqual(self.methods(), ["getMe"])


    def test_status_refuses_mismatch_and_redacts_unexpected_url(self):
        for url in ("", "https://elsewhere.example/opaque-private-path?key=" + SECRET):
            with self.subTest(url=url):
                self.requests.clear()
                self.set_info(url=url)
                code, output, _ = self.invoke("status")
                self.assertEqual(code, 1)
                result = json.loads(output)
                self.assertFalse(result["url_matches"])
                self.assertEqual(result["url"], "[redacted mismatched URL]" if url else "")
                self.assertNotIn("opaque-private-path", output)
                self.assertEqual(self.methods(), ["getWebhookInfo"])

    def test_status_delivery_error_window_requires_pending_updates(self):
        for age, pending, expected in ((0, 1, 1), (599, 3, 1), (600, 1, 1),
                                       (601, 10, 0), (10, 0, 0)):
            with self.subTest(age=age, pending=pending):
                self.set_info(pending_update_count=pending, last_error_date=NOW - age,
                              last_error_message="Wrong response from webhook: 503")
                code, output, _ = self.invoke("status")
                self.assertEqual(code, expected)
                result = json.loads(output)
                self.assertEqual(result["recent_delivery_error"], bool(expected))
                self.assertTrue(result["last_error_at"].endswith("Z"))
                self.assertEqual(result["last_error_message"], "Wrong response from webhook: 503")

    def test_status_redacts_secrets_other_bot_tokens_and_urls_in_delivery_message(self):
        self.set_info(pending_update_count=1, last_error_date=NOW,
                      last_error_message="token " + TOKEN + " secret " + SECRET +
                      " other 123456:another_private_token at https://user:pass@host.test/?secret=opaque\nnext")
        code, output, _ = self.invoke("status")
        self.assertEqual(code, 1)
        result = json.loads(output)
        self.assertNotIn("another_private_token", output)
        self.assertNotIn("opaque", output)
        self.assertNotIn("user:pass", output)
        self.assertNotIn("\n", result["last_error_message"])
        self.assertIn("[redacted", result["last_error_message"])

    def test_malformed_status_fails_without_reflecting_response(self):
        for change in ({"pending_update_count": True}, {"pending_update_count": -1},
                       {"pending_update_count": "2"}, {"last_error_date": TOKEN},
                       {"last_error_date": 10**30}, {"last_error_message": {"secret": SECRET}},
                       {"url": None}):
            with self.subTest(change=change):
                self.set_info(**change)
                code, output, errors = self.invoke("status")
                self.assertEqual(code, 1)
                self.assertEqual(output, "")
                self.assertIn("invalid", errors)

    def test_unknown_arguments_do_not_echo_accidental_token(self):
        code, output, errors = self.invoke(extra=("--token", TOKEN))
        self.assertEqual(code, 2)
        self.assertEqual(output, "")
        self.assertIn("invalid command arguments", errors)
        self.assertEqual(self.requests, [])


if __name__ == "__main__":
    unittest.main()
