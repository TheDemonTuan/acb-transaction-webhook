"""Synthetic model bytes; exercise the real downloader and pinned verification."""
import hashlib
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread
from urllib.parse import urlsplit

import pytest

from scripts import fetch_vieneu_assets as fetch


@pytest.mark.parametrize("corrupt_codec", [False, True])
def test_codec_download_uses_its_own_pinned_metadata(tmp_path, monkeypatch, corrupt_codec):
    files = {"backbone.json": b"backbone", "decoder.onnx": b"decoder", "codec.data": b"weights"}
    preset = json.dumps({"presets": {"Mai Anh": {"gender": "female", "region": "Bắc"}}}).encode()
    monkeypatch.setattr("pkgutil.get_data", lambda *_: preset)

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            name = self.path.rsplit("/", 1)[-1]
            data = files[name]
            if corrupt_codec and name == "codec.data":
                data = b"damaged"
            self.send_response(200)
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *_):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    real_download = fetch.download_file

    def local_download(url, *args):
        return real_download(f"http://127.0.0.1:{server.server_port}{urlsplit(url).path}", *args)

    monkeypatch.setattr(fetch, "download_file", local_download)

    def section(repo, names):
        return {"repo": repo, "revision": "a" * 40, "file_list": names,
                "files": {name: {"sha256": hashlib.sha256(files[name]).hexdigest(), "bytes": len(files[name])}
                          for name in names}}

    manifest = {"backbone": section("fixture/backbone", ["backbone.json"]),
                "codec": section("fixture/codec", ["decoder.onnx", "codec.data"])}
    try:
        if corrupt_codec:
            with pytest.raises(ValueError, match="Hash mismatch for codec.data"):
                fetch.fetch_all(manifest, tmp_path)
            assert not list(tmp_path.rglob("codec.data"))
            assert not list(tmp_path.rglob("*.tmp.*"))
        else:
            fetch.fetch_all(manifest, tmp_path)
            preset_path = tmp_path / "backbone" / "voices_v3_turbo.json"
            manifest["preset"] = {"files": {"voices_v3_turbo.json": {
                "sha256": fetch.sha256_file(preset_path), "bytes": preset_path.stat().st_size}}}
            fetch.verify_manifest(manifest, tmp_path)
            snapshot = tmp_path / "hf/hub/models--fixture--codec/snapshots" / ("a" * 40)
            assert (snapshot / "decoder.onnx").read_bytes() == files["decoder.onnx"]
            assert (snapshot / "codec.data").read_bytes() == files["codec.data"]
    finally:
        server.shutdown()
        thread.join()
        server.server_close()
