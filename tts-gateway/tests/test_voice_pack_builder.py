"""Build tooling tests use stub media and provider bytes, never live synthesis."""
import importlib.util
import hashlib
from pathlib import Path

import pytest

from app.assets import InvalidPack, canonical_json, synthesis_key, validate_manifest
from app.providers.edge import EdgeTTSProvider


SPEC = importlib.util.spec_from_file_location("voice_pack_builder", Path(__file__).parents[1] / "scripts" / "build_voice_pack.py")
builder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(builder)


def inputs():
    return {
        "schemaVersion": 1,
        "synthesisKey": synthesis_key("vi-VN-NamMinhNeural", 1.25, 0),
        "voiceId": "vi-VN-NamMinhNeural", "rate": 1.25, "pitchHz": 0,
        "formatterVersion": "money-vi-v1", "catalogVersion": "v1",
        "clips": [{"clipId": "amount-1", "kind": "amount", "transcript": "Đã nhận một đồng."}],
        "amounts": {"1": {"standard": "amount-1", "classic": {}}},
    }


def test_no_audio_generation_without_reviewed_rights(tmp_path, monkeypatch):
    monkeypatch.setattr("sys.argv", ["builder", "--output", str(tmp_path)])
    called = False

    async def forbidden(*args):
        nonlocal called
        called = True

    monkeypatch.setattr(builder, "build", forbidden)
    with pytest.raises(SystemExit) as error:
        builder.main()
    assert error.value.code == 1
    assert not called
    assert not list(tmp_path.iterdir())


@pytest.mark.parametrize("evidence", [
    {}, {"authorized": False}, {"authorized": True},
    {"authorized": True, "deployment": "fixture", "use": " ", "retention": "reviewed", "redistribution": "reviewed", "evidenceReference": "private approval"},
])
def test_rights_gate_does_not_accept_missing_scope(tmp_path, evidence):
    path = tmp_path / "rights.json"
    path.write_bytes(canonical_json(evidence))
    with pytest.raises(InvalidPack, match="rights gate"):
        builder.require_rights(path)


def test_release_owner_attestation_requires_all_scopes(tmp_path):
    path = tmp_path / "rights.json"
    path.write_bytes(canonical_json({
        "authorized": True, "deployment": "isolated fixture", "use": "reviewed permission",
        "retention": "reviewed permission", "redistribution": "reviewed permission",
        "evidenceReference": "private release evidence (test fixture only)",
    }))
    builder.require_rights(path)


@pytest.mark.asyncio
async def test_exact_profile_build_and_idempotent_resume_use_no_new_synthesis(tmp_path, monkeypatch):
    data = inputs()
    calls = []
    audio = b"deterministic-test-not-MPEG"

    async def stream(self, text, voice, rate, pitch):
        calls.append((text, voice, rate, pitch))
        yield b"synthetic-source"

    monkeypatch.setattr(EdgeTTSProvider, "stream", stream)
    monkeypatch.setattr(builder, "normalize_audio", lambda raw, temporary: (audio, 2000))
    monkeypatch.setattr(builder, "audio_metadata", lambda path: 2000)
    await builder.build(data, tmp_path)
    directory = tmp_path / data["synthesisKey"]
    first = (directory / "manifest.json").read_bytes()
    manifest = validate_manifest(builder.parse_json(first))
    assert manifest["clips"][0]["sha256"] == hashlib.sha256(audio).hexdigest()
    assert calls == [("Đã nhận một đồng.", "vi-VN-NamMinhNeural", "+25%", "+0Hz")]
    assert not (directory / ".build-progress.json").exists()
    await builder.build(data, tmp_path)
    assert len(calls) == 1
    assert (directory / "manifest.json").read_bytes() == first
    # Verification does not invoke providers, even with no Edge access available.
    async def forbidden(*args, **kwargs):
        raise AssertionError("Verification contacted upstream")
        yield b""
    monkeypatch.setattr(EdgeTTSProvider, "stream", forbidden)
    builder.verify_output(tmp_path, data)
    data["clips"][0]["transcript"] = "Different transcript."
    with pytest.raises(InvalidPack, match="exact input"):
        builder.verify_output(tmp_path, data)


@pytest.mark.asyncio
async def test_checkpoint_resume_requires_exact_input_and_valid_hash(tmp_path, monkeypatch):
    data = inputs()
    directory = tmp_path / data["synthesisKey"]
    directory.mkdir()
    checkpoint = directory / ".build-progress.json"
    checkpoint.write_bytes(canonical_json({"inputHash": "0" * 64, "clips": {}}))
    async def forbidden(*args, **kwargs):
        raise AssertionError("Mismatched checkpoint contacted upstream")
        yield b""
    monkeypatch.setattr(EdgeTTSProvider, "stream", forbidden)
    with pytest.raises(InvalidPack, match="Resume inputs differ"):
        await builder.build(data, tmp_path)


def test_initial_input_validation_requires_approved_amounts(tmp_path):
    path = tmp_path / "inputs.json"
    path.write_bytes(canonical_json(inputs()))
    with pytest.raises(InvalidPack, match="twenty approved"):
        builder.load_inputs(path)
