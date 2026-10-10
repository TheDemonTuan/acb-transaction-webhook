#!/usr/bin/env python3
"""Register or inspect the owner-provisioned SePay Telegram receiver.

No polling or startup registration. Secret files must be regular files with mode
0600 on POSIX. Windows operators must restrict their file ACLs to the owner.
"""
import argparse
import base64
import binascii
import datetime
import json
import os
from pathlib import Path
import re
import stat
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

CALLBACK_PATH = "/api/integrations/sepay/telegram"
API_ORIGIN = "https://api.telegram.org"


class OperatorError(Exception):
    """An operator-safe error that never contains input values or API bodies."""


class SafeArgumentParser(argparse.ArgumentParser):
    def error(self, message):
        # argparse's default error can echo an accidentally supplied token.
        super().error("invalid command arguments; use --help")


def read_private_file(path, label):
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_BINARY", 0))
        with os.fdopen(fd, "rb") as stream:
            metadata = os.fstat(stream.fileno())
            if not stat.S_ISREG(metadata.st_mode):
                raise OperatorError(label + " must be a regular file")
            if os.name != "nt" and stat.S_IMODE(metadata.st_mode) != 0o600:
                raise OperatorError(label + " must have mode 0600")
            raw = stream.read(65537)
            if len(raw) > 65536:
                raise OperatorError(label + " is too large")
        return raw.decode("utf-8")
    except (OSError, UnicodeError):
        raise OperatorError("cannot read " + label) from None


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise OperatorError("config file has a duplicate field")
        result[key] = value
    return result


def load_inputs(config_path, token_path):
    try:
        config = json.loads(read_private_file(config_path, "config file"), object_pairs_hook=unique_object)
    except (ValueError, RecursionError):
        raise OperatorError("config file must contain one JSON object") from None
    if not isinstance(config, dict):
        raise OperatorError("config file must contain one JSON object")
    if config.get("mode") not in ("observe", "active"):
        raise OperatorError("config mode must be observe or active")
    bot_id = config.get("botId")
    if type(bot_id) is not int or not 0 < bot_id <= 9223372036854775807:
        raise OperatorError("config botId must be a positive int64")
    secret = config.get("webhookSecret")
    if not isinstance(secret, str) or not re.fullmatch(r"[A-Za-z0-9_-]{43}", secret):
        raise OperatorError("config webhookSecret must encode 32 bytes as unpadded base64url")
    try:
        decoded = base64.b64decode(secret + "=", altchars=b"-_", validate=True)
    except (ValueError, binascii.Error):
        raise OperatorError("invalid config webhookSecret") from None
    if len(decoded) != 32 or base64.urlsafe_b64encode(decoded).decode().rstrip("=") != secret:
        raise OperatorError("invalid config webhookSecret")
    token = read_private_file(token_path, "token file").strip()
    if not re.fullmatch(r"[0-9]+:[A-Za-z0-9_-]+", token):
        raise OperatorError("token file must contain a BotFather token")
    return config, token


def public_origin(value):
    try:
        parsed = urllib.parse.urlsplit(value)
        port = parsed.port
        if (parsed.scheme != "https" or not parsed.hostname or parsed.username is not None
                or parsed.password is not None or parsed.path or parsed.query or parsed.fragment
                or "?" in value or "#" in value or "\\" in value
                or any(character.isspace() or ord(character) < 32 for character in value)
                or (port is not None and not 0 < port <= 65535)
                or parsed.netloc.endswith(":")):
            raise ValueError
        # A DNS host or bracketed IPv6 address; urllib accepts invalid DNS punctuation.
        if not re.fullmatch(r"[A-Za-z0-9.-]+|[0-9A-Fa-f:]+", parsed.hostname):
            raise ValueError
    except ValueError:
        raise OperatorError("public origin must be HTTPS without userinfo, path, query, or fragment") from None
    return value


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class TelegramAPI:
    def __init__(self, token, opener=None):
        self.token = token
        self.opener = opener or urllib.request.build_opener(NoRedirect())

    def call(self, method, parameters=None):
        # The token only appears in Telegram's required HTTPS request path, never
        # in argv, diagnostics, response output, or a persisted log.
        request = urllib.request.Request(
            API_ORIGIN + "/bot" + self.token + "/" + method,
            data=json.dumps(parameters or {}).encode(),
            headers={"Content-Type": "application/json"}, method="POST")
        try:
            with self.opener.open(request, timeout=20) as response:
                raw = response.read(65537)
            if len(raw) > 65536:
                raise ValueError
            body = json.loads(raw)
            if not isinstance(body, dict) or body.get("ok") is not True or "result" not in body:
                raise ValueError
        except (OSError, ValueError, RecursionError, urllib.error.URLError):
            raise OperatorError("Telegram " + method + " request failed") from None
        return body["result"]


def webhook_info(api):
    info = api.call("getWebhookInfo")
    if not isinstance(info, dict) or not isinstance(info.get("url"), str):
        raise OperatorError("Telegram getWebhookInfo returned an invalid result")
    return info


def register(api, config, expected_url):
    identity = api.call("getMe")
    if (not isinstance(identity, dict) or type(identity.get("id")) is not int
            or identity["id"] != config["botId"] or identity.get("is_bot") is not True):
        raise OperatorError("Telegram bot identity does not match config botId")
    current = webhook_info(api)["url"]
    if current and current != expected_url:
        raise OperatorError("existing webhook URL differs; refusing to replace another receiver")
    result = api.call("setWebhook", {
        "url": expected_url,
        "secret_token": config["webhookSecret"],
        "allowed_updates": ["message", "edited_message"],
        "max_connections": 4,
        "drop_pending_updates": False,
    })
    if result is not True:
        raise OperatorError("Telegram did not confirm webhook registration")
    return {"url": expected_url, "registered": True}


def redact(value, token, secret):
    text = value.replace(token, "[redacted]").replace(secret, "[redacted]")
    text = re.sub(r"(?i)\bhttps?://\S+", "[redacted URL]", text)
    text = re.sub(r"\b(?:bot)?[0-9]+:[A-Za-z0-9_-]+", "[redacted token]", text)
    text = "".join(character if character.isprintable() else " " for character in text)
    return text[:512]


def display_url(url, expected_url):
    if url == expected_url:
        return expected_url
    # An unexpected URL can contain credentials, secret query parameters, or
    # opaque tokens in its path. Do not reflect it to an operator's terminal.
    return "[redacted mismatched URL]" if url else ""


def status(api, config, expected_url, now):
    info = webhook_info(api)
    pending = info.get("pending_update_count")
    timestamp = info.get("last_error_date")
    message = info.get("last_error_message", "")
    if (type(pending) is not int or pending < 0 or
            (timestamp is not None and (type(timestamp) is not int or timestamp < 0)) or
            not isinstance(message, str)):
        raise OperatorError("Telegram getWebhookInfo returned an invalid result")
    try:
        error_at = (datetime.datetime.fromtimestamp(timestamp, datetime.timezone.utc)
                    .isoformat().replace("+00:00", "Z") if timestamp is not None else None)
    except (ValueError, OverflowError, OSError):
        raise OperatorError("Telegram getWebhookInfo returned an invalid error timestamp") from None
    recent_delivery_error = timestamp is not None and timestamp >= now - 600 and pending > 0
    matches = info["url"] == expected_url
    return {
        "url": display_url(info["url"], expected_url),
        "url_matches": matches,
        "pending_update_count": pending,
        "last_error_at": error_at,
        "last_error_message": redact(message, api.token, config["webhookSecret"]),
        "recent_delivery_error": recent_delivery_error,
    }, 0 if matches and not recent_delivery_error else 1


def main(argv=None, api_factory=TelegramAPI, now=None):
    parser = SafeArgumentParser(description=__doc__, allow_abbrev=False)
    parser.add_argument("command", choices=("register", "status"))
    parser.add_argument("--config-file", type=Path, required=True)
    parser.add_argument("--token-file", type=Path, required=True)
    parser.add_argument("--public-origin", required=True)
    args = parser.parse_args(argv)
    try:
        origin = public_origin(args.public_origin)
        config, token = load_inputs(args.config_file, args.token_file)
        api = api_factory(token)
        expected_url = origin + CALLBACK_PATH
        if args.command == "register":
            result, code = register(api, config, expected_url), 0
        else:
            result, code = status(api, config, expected_url, time.time() if now is None else now)
        output = json.dumps(result, ensure_ascii=True)
        print(output.replace(token, "[redacted]").replace(config["webhookSecret"], "[redacted]"))
        return code
    except OperatorError as error:
        print("error: " + str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
