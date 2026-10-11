"""Synthetic bytes only: these fixtures are not licensed or publishable packs."""
import hashlib

import pytest
from httpx import ASGITransport, AsyncClient

import app.main as gateway
from app.assets import (
    AssetStore, InvalidPack, MAX_CLIP_BYTES, MAX_MANIFEST_BYTES, MAX_PACK_BYTES,
    canonical_json, parse_json, synthesis_key, validate_manifest,
)
from app.config import config


AUDIO = b"\xff\xf3\x84\x00deterministic-not-real-audio"


def sign_manifest(manifest):
    unsigned = {key: value for key, value in manifest.items() if key != "packVersion"}
    manifest["packVersion"] = hashlib.sha256(canonical_json(unsigned)).hexdigest()
    return manifest


def publish(root, voice="vi-VN-NamMinhNeural", rate=1.25, pitch=0):
    key = synthesis_key(voice, rate, pitch)
    digest = hashlib.sha256(AUDIO).hexdigest()
    manifest = sign_manifest({
        "schemaVersion": 1, "synthesisKey": key, "voiceId": voice,
        "rate": rate, "pitchHz": pitch, "formatterVersion": "money-vi-v1",
        "catalogVersion": "v1", "clips": [{
            "clipId": "amount-500000", "kind": "amount",
            "transcript": "Đã nhận năm trăm nghìn đồng.",
            "durationMs": 2000, "sampleRate": 24000,
            "bytes": len(AUDIO), "sha256": digest,
        }], "amounts": {"500000": {"standard": "amount-500000", "classic": {}}},
    })
    (root / key).mkdir(parents=True)
    (root / "clips").mkdir(exist_ok=True)
    (root / key / "manifest.json").write_bytes(canonical_json(manifest))
    (root / "clips" / (digest + ".mp3")).write_bytes(AUDIO)
    return key, digest, manifest


@pytest.fixture
def published(tmp_path, monkeypatch):
    key, digest, manifest = publish(tmp_path)
    monkeypatch.setattr(gateway, "assets", AssetStore(tmp_path))
    monkeypatch.setattr(config, "require_auth", True)
    monkeypatch.setattr(config, "internal_token", "asset-test-token")
    monkeypatch.setattr(config, "token_file", "")
    return tmp_path, key, digest, manifest


@pytest.mark.asyncio
async def test_asset_endpoints_require_existing_internal_auth(published):
    _, key, digest, _ = published
    async with AsyncClient(transport=ASGITransport(app=gateway.app), base_url="http://test") as client:
        for route in (f"/assets/profiles/{key}/manifest", f"/assets/clips/{digest}"):
            assert (await client.get(route)).status_code == 401
            assert (await client.get(route, headers={"X-Internal-TTS-Token": "wrong"})).status_code == 401
            assert (await client.post(route, headers={"X-Internal-TTS-Token": "asset-test-token"})).status_code == 405


@pytest.mark.asyncio
async def test_exact_profile_manifest_etag_and_authorized_clip(published):
    _, key, digest, manifest = published
    headers = {"X-Internal-TTS-Token": "asset-test-token"}
    async with AsyncClient(transport=ASGITransport(app=gateway.app), base_url="http://test") as client:
        result = await client.get(f"/assets/profiles/{key}/manifest", headers=headers)
        assert result.status_code == 200
        assert result.content == canonical_json(manifest)
        assert result.headers["etag"] == '"' + manifest["packVersion"] + '"'
        assert result.headers["cache-control"] == "no-cache"
        not_modified = await client.get(f"/assets/profiles/{key}/manifest", headers=dict(headers, **{"If-None-Match": result.headers["etag"]}))
        assert not_modified.status_code == 304
        assert not not_modified.content
        clip = await client.get(f"/assets/clips/{digest}", headers=headers)
        assert clip.content == AUDIO
        assert clip.headers["content-type"] == "audio/mpeg"
        assert clip.headers["cache-control"] == "public,max-age=31536000,immutable"
        other_profile = synthesis_key("vi-VN-NamMinhNeural", 1.2, 0)
        assert (await client.get(f"/assets/profiles/{other_profile}/manifest", headers=headers)).status_code == 404
        assert (await client.get("/assets/clips/" + "A" * 64, headers=headers)).status_code == 404


def test_unlisted_content_addressed_file_is_not_published(tmp_path):
    publish(tmp_path)
    extra = b"unlisted bytes"
    digest = hashlib.sha256(extra).hexdigest()
    (tmp_path / "clips" / (digest + ".mp3")).write_bytes(extra)
    with pytest.raises(KeyError):
        AssetStore(tmp_path).clip(digest)


@pytest.mark.parametrize("damage", ["hash", "profile", "clip", "missing", "oversize", "duplicate-key"])
def test_corrupt_pack_is_entirely_unavailable(tmp_path, damage):
    key, digest, manifest = publish(tmp_path)
    path = tmp_path / key / "manifest.json"
    clip = tmp_path / "clips" / (digest + ".mp3")
    if damage == "hash":
        manifest["clips"][0]["transcript"] = "altered transcript"
        path.write_bytes(canonical_json(manifest))
    elif damage == "profile":
        manifest["rate"] = 1.2
        path.write_bytes(canonical_json(sign_manifest(manifest)))
    elif damage == "clip":
        clip.write_bytes(b"x" * len(AUDIO))
    elif damage == "missing":
        clip.unlink()
    elif damage == "oversize":
        path.write_bytes(b" " * (MAX_MANIFEST_BYTES + 1))
    else:
        path.write_bytes(b'{"schemaVersion":1,"schemaVersion":1}')
    store = AssetStore(tmp_path)
    with pytest.raises(KeyError):
        store.manifest(key)
    with pytest.raises(KeyError):
        store.clip(digest)


@pytest.mark.asyncio
async def test_corruption_after_load_invalidates_pack_not_wrong_audio(published):
    root, key, digest, _ = published
    gateway.assets.manifest(key)
    (root / "clips" / (digest + ".mp3")).write_bytes(b"x" * len(AUDIO))
    headers = {"X-Internal-TTS-Token": "asset-test-token"}
    async with AsyncClient(transport=ASGITransport(app=gateway.app), base_url="http://test") as client:
        assert (await client.get(f"/assets/clips/{digest}", headers=headers)).status_code == 404
        assert (await client.get(f"/assets/profiles/{key}/manifest", headers=headers)).status_code == 404


@pytest.mark.parametrize("field,value", [
    ("bytes", MAX_CLIP_BYTES + 1), ("bytes", 0), ("bytes", True),
    ("durationMs", 0), ("sampleRate", 48000), ("sha256", "../escape"),
])
def test_metadata_limits_and_hash_shape(tmp_path, field, value):
    _, _, manifest = publish(tmp_path)
    manifest["clips"][0][field] = value
    with pytest.raises(InvalidPack):
        validate_manifest(sign_manifest(manifest))


def test_total_pack_limit(tmp_path):
    _, _, manifest = publish(tmp_path)
    original = manifest["clips"][0]
    manifest["clips"] = [dict(original, clipId=f"amount-{index}", bytes=MAX_CLIP_BYTES) for index in range(MAX_PACK_BYTES // MAX_CLIP_BYTES + 1)]
    manifest["amounts"] = {"1": {"standard": "amount-0", "classic": {}}}
    with pytest.raises(InvalidPack, match="Pack exceeds"):
        validate_manifest(sign_manifest(manifest))


def test_symlink_asset_not_served(tmp_path):
    key, digest, _ = publish(tmp_path)
    target = tmp_path / "clips" / (digest + ".mp3")
    outside = tmp_path / "not-published.mp3"
    outside.write_bytes(AUDIO)
    target.unlink()
    try:
        target.symlink_to(outside)
    except OSError:
        pytest.skip("Platform does not permit test symlinks")
    with pytest.raises(KeyError):
        AssetStore(tmp_path).manifest(key)


def test_canonical_hash_and_exact_synthesis_key():
    canonical = "edge|vi-VN-NamMinhNeural|1.25|0|money-vi-v1|pack-schema-1"
    assert synthesis_key("vi-VN-NamMinhNeural", 1.25, 0) == hashlib.sha256(canonical.encode()).hexdigest()
    assert canonical_json({"z": "đồng", "a": 1}) == '{"a":1,"z":"đồng"}'.encode()
    assert canonical_json({"rate": 1.0}) == b'{"rate":1}'
    for raw in (b'{"a":1,"a":2}', b'{"a":NaN}', b'{} trailing'):
        with pytest.raises(InvalidPack):
            parse_json(raw)


@pytest.mark.parametrize("rate,pitch", [(1.251, 0), (0.74, 0), (2.01, 0), (True, 0), (1.25, True), (1.25, 51)])
def test_profile_key_rejects_noncanonical_values(rate, pitch):
    with pytest.raises(InvalidPack):
        synthesis_key("vi-VN-NamMinhNeural", rate, pitch)


@pytest.mark.asyncio
async def test_asset_auth_remains_required_when_development_synthesis_auth_is_disabled(published, monkeypatch):
    _, key, digest, _ = published
    monkeypatch.setattr(config, "require_auth", False)
    monkeypatch.setattr(config, "internal_token", "")
    async with AsyncClient(transport=ASGITransport(app=gateway.app), base_url="http://test") as client:
        assert (await client.get(f"/assets/profiles/{key}/manifest")).status_code == 503
        assert (await client.get(f"/assets/clips/{digest}")).status_code == 503
