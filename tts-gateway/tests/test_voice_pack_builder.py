"""Synthetic publication contracts plus real candidate synthesis, never human QA."""
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
        "schemaVersion": 2,
        "synthesisKey": synthesis_key("vi-VN-NamMinhNeural", 1.25, 0),
        "voiceId": "vi-VN-NamMinhNeural", "rate": 1.25, "pitchHz": 0,
        "formatterVersion": "money-vi-v1", "catalogVersion": "v1",
        "preloadClipIds": ["amount-1"],
        "numberSegments": None,
        "numberPrefix": None,
        "numberSuffix": None,
        "clips": [{"clipId": "amount-1", "kind": "amount", "transcript": "Đã nhận một đồng."}],
        "amounts": {"1": {"standard": "amount-1", "classic": {}}},
    }


def synthetic_review(data, candidate):
    """Schema fixture only; not evidence that a person heard real audio."""
    return {
        "schemaVersion": 1, "decision": "approved", "synthesisKey": data["synthesisKey"],
        "inputHash": hashlib.sha256(canonical_json(data)).hexdigest(),
        "reviewer": "synthetic unit fixture (not release evidence)", "reviewedAt": "2026-10-12T00:00:00Z",
        "tailMode": "whole", "numberMode": "whole", "candidateDecisions": {"tail": "rejected", "numbers": "rejected"},
        "clips": [{key: clip[key] for key in ("clipId", "sha256", "speechStartMs", "amountStartMs")} for clip in candidate["clips"]],
        "heardClipIds": [clip["clipId"] for clip in candidate["clips"]], "heardRecipePairIds": [],
        "amountAccuracy": {"correct": 50, "total": 50}, "readabilityMedian": 4, "naturalnessMedian": 4,
        "joinFailures": [], "rejectedClipIds": [], "selectedRate": data["rate"], "sampleSetSha256": "a" * 64,
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
    candidate_bytes = (directory / "manifest.candidate.json").read_bytes()
    assert candidate_bytes
    evidence = synthetic_review(data, builder.parse_json(candidate_bytes))
    evidence_path = tmp_path / "evidence.json"
    evidence_path.write_bytes(canonical_json(evidence))
    builder.publish(data, tmp_path, evidence_path)
    first = (directory / "manifest.json").read_bytes()
    manifest = validate_manifest(builder.parse_json(first))
    assert manifest["clips"][0]["sha256"] == hashlib.sha256(audio).hexdigest()
    assert calls == [("Đã nhận một đồng.", "vi-VN-NamMinhNeural", "+25%", "+0Hz")]
    assert not (directory / ".build-progress.json").exists()
    await builder.build(data, tmp_path)
    assert len(calls) == 1
    assert (directory / "manifest.json").read_bytes() == first
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
    with pytest.raises(InvalidPack, match="base approved"):
        builder.load_inputs(path)


def test_piper_license_gate_uses_original_model_card_not_edge_attestation(tmp_path):
    card = Path(__file__).parents[1] / "assets" / "models" / "MODEL_CARD"
    builder.require_piper_license(card)
    with pytest.raises(InvalidPack, match="model-card required"):
        builder.require_piper_license(None)
    forged = tmp_path / "MODEL_CARD"
    forged.write_text("CC-BY-4.0 vais1000 authorized=true", encoding="utf-8")
    with pytest.raises(InvalidPack, match="pinned upstream"):
        builder.require_piper_license(forged)


def test_build_error_keeps_nested_cause():
    error = ExceptionGroup("worker failure", [ValueError("specific cause")])
    assert builder.failure_details(error) == "ValueError: specific cause"


@pytest.mark.asyncio
async def test_real_piper_pack_build_and_verify(tmp_path):
    import shutil
    if not shutil.which("ffmpeg") or not shutil.which("ffprobe"):
        pytest.skip("Build-only ffmpeg/ffprobe required for real normalization")
    data = inputs()
    data["voiceId"] = "vi-VN-vais1000-medium"
    data["synthesisKey"] = synthesis_key(data["voiceId"], data["rate"], 0)
    await builder.build(data, tmp_path)
    directory = tmp_path / data["synthesisKey"]
    manifest = builder.parse_json((directory / "manifest.candidate.json").read_bytes())
    assert manifest["voiceId"] == data["voiceId"]
    assert manifest["clips"][0]["sampleRate"] == 24000
    assert manifest["clips"][0]["transcript"] == "Đã nhận một đồng."
    evidence_path = tmp_path / "evidence.json"
    evidence_path.write_bytes(canonical_json({"decision": "approved", "clips": []}))
    with pytest.raises(InvalidPack):
        builder.publish(data, tmp_path, evidence_path)
    assert not (directory / "manifest.json").exists()
    assert not (directory / "quality.json").exists()


@pytest.mark.parametrize("input_i", ["-18.02", "-inf"])
def test_loudnorm_json_followed_by_ffmpeg7_progress(monkeypatch, input_i):
    from types import SimpleNamespace
    measurements = {"input_i": input_i, "input_tp": "-3.85", "input_lra": "0.00", "input_thresh": "-28.02", "target_offset": "0.02"}
    stderr = "[Parsed_loudnorm_0] \n" + canonical_json(measurements).decode() + "\n[out#0/null] video:0KiB audio:387KiB\nsize=N/A time=00:00:01.03\n"
    monkeypatch.setattr(builder, "run_media", lambda arguments: SimpleNamespace(stderr=stderr))
    if input_i == "-inf":
        with pytest.raises(InvalidPack, match="Invalid loudness"):
            builder.loudness(Path("fixture.wav"))
    else:
        assert builder.loudness(Path("fixture.wav")) == measurements


def candidate_fixture(root):
    data = inputs()
    audio = b"synthetic-publication-fixture"
    clip = dict(data["clips"][0], sha256=hashlib.sha256(audio).hexdigest(), bytes=len(audio), durationMs=2000,
                sampleRate=24000, speechStartMs=None, amountStartMs=None)
    candidate = dict(data, clips=[clip], composition={"tail": "whole", "numbers": "whole"})
    candidate["packVersion"] = hashlib.sha256(canonical_json(candidate)).hexdigest()
    directory = root / data["synthesisKey"]
    directory.mkdir()
    (root / "clips").mkdir()
    (root / "clips" / (clip["sha256"] + ".mp3")).write_bytes(audio)
    (directory / "manifest.candidate.json").write_bytes(canonical_json(candidate))
    return data, candidate, directory


@pytest.mark.parametrize("damage", ["empty-clips", "missing-review", "unheard", "duplicate", "hash", "pending", "scores", "rate", "candidate-input"])
def test_publish_never_approves_incomplete_or_mismatched_review(tmp_path, damage, monkeypatch):
    data, candidate, directory = candidate_fixture(tmp_path)
    evidence = synthetic_review(data, candidate)
    if damage == "empty-clips":
        evidence["clips"] = []
    elif damage == "missing-review":
        del evidence["reviewer"]
    elif damage == "unheard":
        evidence["heardClipIds"] = []
    elif damage == "duplicate":
        evidence["clips"].append(dict(evidence["clips"][0]))
    elif damage == "hash":
        evidence["clips"][0]["sha256"] = "f" * 64
    elif damage == "pending":
        evidence["decision"] = "pending"
    elif damage == "scores":
        evidence["naturalnessMedian"] = None
    elif damage == "rate":
        evidence["selectedRate"] = 1.1
    else:
        candidate["clips"][0]["transcript"] = "Unreviewed changed text"
        (directory / "manifest.candidate.json").write_bytes(canonical_json(candidate))
    evidence_path = tmp_path / "review.json"
    evidence_path.write_bytes(canonical_json(evidence))
    monkeypatch.setattr(builder, "audio_metadata", lambda path: 2000)
    with pytest.raises(InvalidPack):
        builder.publish(data, tmp_path, evidence_path)
    assert not (directory / "manifest.json").exists()
    assert not (directory / "quality.json").exists()


def test_publish_is_idempotent_but_never_overwrites_existing_review(tmp_path, monkeypatch):
    data, candidate, directory = candidate_fixture(tmp_path)
    evidence = synthetic_review(data, candidate)
    evidence_path = tmp_path / "review.json"
    evidence_path.write_bytes(canonical_json(evidence))
    monkeypatch.setattr(builder, "audio_metadata", lambda path: 2000)
    builder.publish(data, tmp_path, evidence_path)
    original = [(directory / name).read_bytes() for name in ("manifest.json", "quality.json")]
    builder.publish(data, tmp_path, evidence_path)
    evidence["reviewer"] = "another synthetic fixture"
    evidence_path.write_bytes(canonical_json(evidence))
    with pytest.raises(InvalidPack, match="immutable"):
        builder.publish(data, tmp_path, evidence_path)
    assert original == [(directory / name).read_bytes() for name in ("manifest.json", "quality.json")]
