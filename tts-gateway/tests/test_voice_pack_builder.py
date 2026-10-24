"""Synthetic publication contracts plus real candidate synthesis, never human QA."""
import importlib.util
import hashlib
from pathlib import Path

import pytest

from app.assets import InvalidPack, VIENEU_VOICE, canonical_json, synthesis_key, validate_manifest
from app.providers.vieneu import VieneuTTSProvider


SPEC = importlib.util.spec_from_file_location("voice_pack_builder", Path(__file__).parents[1] / "scripts" / "build_voice_pack.py")
builder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(builder)


def inputs():
    return {
        "schemaVersion": 2,
        "synthesisKey": synthesis_key(VIENEU_VOICE, 1.0, 0),
        "voiceId": VIENEU_VOICE, "rate": 1.0, "pitchHz": 0,
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


def test_no_audio_generation_without_inputs(tmp_path, monkeypatch):
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


def test_vieneu_license_gate_requires_pinned_upstream_model_card(tmp_path):
    card = Path(__file__).parents[1] / "assets" / "models" / "vieneu" / "backbone" / "README.md"
    builder.require_vieneu_license(card)
    with pytest.raises(InvalidPack, match="model-card required"):
        builder.require_vieneu_license(None)
    forged = tmp_path / "README.md"
    forged.write_text("VieNeu-TTS authorized=true", encoding="utf-8")
    with pytest.raises(InvalidPack, match="pinned upstream"):
        builder.require_vieneu_license(forged)


@pytest.fixture
def synthetic_provider_lifecycle(monkeypatch):
    async def idle(self):
        pass
    monkeypatch.setattr(VieneuTTSProvider, "start", idle)
    monkeypatch.setattr(VieneuTTSProvider, "close", idle)


@pytest.mark.parametrize("publish", [False, True])
def test_generation_and_publication_require_vieneu_license(tmp_path, monkeypatch, publish):
    arguments = ["builder", "--inputs", "inputs.json", "--output", str(tmp_path)]
    if publish:
        arguments.append("--publish")
    monkeypatch.setattr("sys.argv", arguments)
    monkeypatch.setattr(builder, "load_inputs", lambda path: inputs())
    with pytest.raises(SystemExit) as error:
        builder.main()
    assert error.value.code == 1
    assert not list(tmp_path.iterdir())


def approved_inputs():
    data = inputs()
    data["clips"] = []
    data["amounts"] = {}
    for amount in sorted(builder.BASE_APPROVED_AMOUNTS):
        standard = f"amount-{amount}"
        data["clips"].append({"clipId": standard, "kind": "amount", "transcript": f"Số tiền {amount} đồng."})
        classics = {}
        for index in range(1, 7):
            phrase = f"classic-{index:03d}"
            cid = f"{phrase}-{amount}"
            classics[phrase] = cid
            data["clips"].append({"clipId": cid, "kind": "classic", "transcript": f"Câu {index}, số tiền {amount} đồng."})
        data["amounts"][amount] = {"standard": standard, "classic": classics}
    data["preloadClipIds"] = [data["clips"][0]["clipId"]]
    return data


def test_initial_vieneu_pack_contains_exact_whole_utterances(tmp_path):
    data = approved_inputs()
    path = tmp_path / "inputs.json"
    path.write_bytes(canonical_json(data))
    assert builder.load_inputs(path) == data


@pytest.mark.parametrize("damage", ["classic", "extra", "fragments", "segments", "profile", "schema"])
def test_initial_vieneu_pack_retains_profile_schema_and_whole_utterance_gates(tmp_path, damage):
    data = approved_inputs()
    if damage == "classic":
        next(iter(data["amounts"].values()))["classic"].pop("classic-006")
    elif damage == "extra":
        data["clips"].append({"clipId": "extra", "kind": "tail-full", "transcript": "Cảm ơn."})
    elif damage in ("fragments", "segments"):
        data["clips"].append({"clipId": "composed", "kind": "tail-fragment" if damage == "fragments" else "amount-segment", "transcript": "Một."})
    elif damage == "profile":
        data["voiceId"] = "vi-VN-NamMinhNeural"
    else:
        data["schemaVersion"] = 1
    path = tmp_path / "inputs.json"
    path.write_bytes(canonical_json(data))
    with pytest.raises(InvalidPack):
        builder.load_inputs(path)


@pytest.mark.asyncio
async def test_exact_profile_build_and_idempotent_resume_use_no_new_synthesis(tmp_path, monkeypatch, synthetic_provider_lifecycle):
    data = inputs()
    calls = []
    audio = b"deterministic-test-not-MPEG"

    async def stream(self, text, voice, rate, pitch):
        calls.append((text, voice, rate, pitch))
        yield b"synthetic-source"

    monkeypatch.setattr(VieneuTTSProvider, "stream", stream)
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
    assert calls == [("Đã nhận một đồng.", VIENEU_VOICE, "+0%", "+0Hz")]
    assert not (directory / ".build-progress.json").exists()
    await builder.build(data, tmp_path)
    assert len(calls) == 1
    assert (directory / "manifest.json").read_bytes() == first
    async def forbidden(*args, **kwargs):
        raise AssertionError("Verification contacted upstream")
        yield b""
    monkeypatch.setattr(VieneuTTSProvider, "stream", forbidden)
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
    monkeypatch.setattr(VieneuTTSProvider, "stream", forbidden)
    with pytest.raises(InvalidPack, match="Resume inputs differ"):
        await builder.build(data, tmp_path)


def test_initial_input_validation_requires_approved_amounts(tmp_path):
    path = tmp_path / "inputs.json"
    path.write_bytes(canonical_json(inputs()))
    with pytest.raises(InvalidPack, match="base approved"):
        builder.load_inputs(path)




def test_build_error_keeps_nested_cause():
    error = ExceptionGroup("worker failure", [ValueError("specific cause")])
    assert builder.failure_details(error) == "ValueError: specific cause"


@pytest.mark.asyncio
async def test_real_vieneu_pack_build_and_verify(tmp_path):
    import shutil
    if not shutil.which("ffmpeg") or not shutil.which("ffprobe"):
        pytest.skip("Build-only ffmpeg/ffprobe required for real normalization")
    data = inputs()
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


@pytest.mark.parametrize("damage", ["empty-clips", "missing-review", "unheard", "duplicate", "hash", "pending", "scores", "rate", "candidate-input", "amount-errors", "amount-count", "composed-tail", "composed-numbers"])
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
    elif damage == "amount-errors":
        evidence["amountAccuracy"]["correct"] = 49
    elif damage == "amount-count":
        evidence["amountAccuracy"] = {"correct": 49, "total": 49}
    elif damage == "composed-tail":
        evidence["tailMode"] = "fragments"
        evidence["candidateDecisions"]["tail"] = "approved"
    elif damage == "composed-numbers":
        evidence["numberMode"] = "segments"
        evidence["candidateDecisions"]["numbers"] = "approved"
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


@pytest.mark.asyncio
@pytest.mark.parametrize("field,value", [("voiceId", "vi-VN-NamMinhNeural"), ("voiceId", "vi-VN-vais1000-medium"), ("rate", 1.25), ("pitchHz", 5)])
async def test_direct_build_rejects_retired_profiles_before_provider_creation(tmp_path, monkeypatch, field, value):
    data = inputs()
    data[field] = value

    def forbidden(*args, **kwargs):
        raise AssertionError("Invalid profile constructed a synthesis provider")

    monkeypatch.setattr("app.providers.vieneu.VieneuTTSProvider", forbidden)
    with pytest.raises(InvalidPack):
        await builder.build(data, tmp_path)
    assert not list(tmp_path.iterdir())
