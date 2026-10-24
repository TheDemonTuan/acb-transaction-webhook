"""Synthetic CLI/evidence contracts; these fixtures are never human release QA."""
import hashlib
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace

import pytest

from app.assets import VIENEU_VOICE, canonical_json, synthesis_key


SPEC = importlib.util.spec_from_file_location("voice_quality", Path(__file__).parents[1] / "scripts" / "voice_quality.py")
quality = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(quality)


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False), encoding="utf-8")
    return path


def test_prepare_cli_defaults_to_fixed_vieneu(tmp_path, monkeypatch):
    captured = []

    async def capture(args):
        captured.append(args)

    monkeypatch.setattr(quality, "run_prepare", capture)
    monkeypatch.setattr("sys.argv", ["voice_quality", "prepare", "--corpus", "corpus.json", "--output", str(tmp_path)])
    quality.main()
    assert len(captured) == 1
    assert captured[0].provider == "vieneu"
    assert captured[0].rates == "1.0"


@pytest.mark.parametrize("provider", ["edge", "piper", "gtts"])
def test_prepare_cli_rejects_retired_providers(tmp_path, monkeypatch, provider):
    monkeypatch.setattr("sys.argv", ["voice_quality", "prepare", "--provider", provider, "--corpus", "corpus.json", "--output", str(tmp_path)])
    with pytest.raises(SystemExit) as error:
        quality.main()
    assert error.value.code == 2


def test_retired_comparison_cli_is_not_available(monkeypatch):
    monkeypatch.setattr("sys.argv", ["voice_quality", "render-comparisons"])
    with pytest.raises(SystemExit) as error:
        quality.main()
    assert error.value.code == 2


@pytest.mark.asyncio
@pytest.mark.parametrize("rates", ["1.25", "0.8,1.0", "1.0,1.0"])
async def test_prepare_rejects_nonfixed_rates_before_provider_start(tmp_path, monkeypatch, rates):
    corpus = write_json(tmp_path / "corpus.json", {"cases": []})

    def forbidden(**kwargs):
        pytest.fail("Invalid rate must not construct a provider")

    monkeypatch.setattr(quality, "VieneuTTSProvider", forbidden)
    with pytest.raises(ValueError, match="only supports rate 1.0"):
        await quality.run_prepare(SimpleNamespace(corpus=corpus, output=tmp_path / "listening", provider="vieneu", rates=rates))


@pytest.mark.asyncio
async def test_prepare_generates_only_fixed_vieneu_samples(tmp_path, monkeypatch):
    cases = [{"id": f"amount-{amount}", "kind": "amount", "amountVnd": amount, "transcript": f"Số tiền {amount} đồng."} for amount in quality.CANONICAL_AMOUNTS_50]
    corpus = write_json(tmp_path / "corpus.json", {"cases": cases})
    calls = []

    class SyntheticProvider:
        def __init__(self, *, threads):
            assert threads == 2

        async def start(self):
            calls.append("start")

        async def close(self):
            calls.append("close")

        async def synthesize(self, text, *, voice, rate):
            calls.append((text, voice, rate))
            return b"synthetic unit fixture, not playable release audio"

    monkeypatch.setattr(quality, "VieneuTTSProvider", SyntheticProvider)
    output = tmp_path / "listening"
    await quality.run_prepare(SimpleNamespace(corpus=corpus, output=output, provider="vieneu", rates="1.0", all_cases=False))
    assert calls[0] == "start" and calls[-1] == "close"
    assert len(calls) == 52
    assert all(voice == VIENEU_VOICE and rate == "+0%" for _, voice, rate in calls[1:-1])
    mapping = json.loads((output / "mapping.sealed.json").read_text(encoding="utf-8"))
    assert mapping["providerMode"] == "VIENEU_ONLY"
    assert mapping["voiceId"] == VIENEU_VOICE
    assert mapping["rates"] == [1.0]
    assert len(mapping["items"]) == 50
    review = json.loads((output / "review.json").read_text(encoding="utf-8"))
    assert review["reviewer"] == ""
    assert all(item["readability"] is None and item["heardAmount"] == "" for item in review["reviews"])


@pytest.fixture
def candidate_pack(tmp_path):
    root = tmp_path / "candidate"
    (root / "clips").mkdir(parents=True)
    raw = b"synthetic pack checksum fixture; not release audio"
    digest = hashlib.sha256(raw).hexdigest()
    (root / "clips" / f"{digest}.mp3").write_bytes(raw)
    inputs = {
        "schemaVersion": 2,
        "synthesisKey": synthesis_key(VIENEU_VOICE, 1.0, 0),
        "voiceId": VIENEU_VOICE, "rate": 1.0, "pitchHz": 0,
        "formatterVersion": "money-vi-v1", "catalogVersion": "v1",
        "preloadClipIds": ["amount-1000"],
        "clips": [{"clipId": "amount-1000", "kind": "amount", "transcript": "Đã nhận một nghìn đồng."}],
        "amounts": {"1000": {"standard": "amount-1000", "classic": {}}},
    }
    manifest = {**inputs, "packVersion": "a" * 64, "composition": {"tail": "whole", "numbers": "whole"},
                "clips": [{**inputs["clips"][0], "sha256": digest, "bytes": len(raw)}]}
    write_json(root / "manifest.candidate.json", manifest)
    return root, inputs


def test_pack_review_starts_with_unfilled_human_amount_counts(tmp_path, candidate_pack):
    root, inputs = candidate_pack
    corpus_data = {"cases": [{"id": "amount-1000", "amountVnd": "1000"}]}
    corpus = write_json(tmp_path / "corpus.json", corpus_data)
    output = tmp_path / "review"
    quality.run_prepare_pack_review(SimpleNamespace(pack=root, corpus=corpus, output=output))
    review = json.loads((output / "review.json").read_text(encoding="utf-8"))
    assert review["amountAccuracy"] == {"correct": None, "total": None}
    assert review["reviewer"] == "" and review["reviewedAt"] == ""
    assert review["inputHash"] == hashlib.sha256(canonical_json(inputs)).hexdigest()
    assert review["sampleSetSha256"] == hashlib.sha256(canonical_json(corpus_data)).hexdigest()
    assert all(item["approved"] is None for item in review["reviews"])
    with pytest.raises(ValueError, match="Refusing to overwrite"):
        quality.run_prepare_pack_review(SimpleNamespace(pack=root, corpus=corpus, output=output))


def synthetic_pack_review():
    return {
        "schemaVersion": 2, "synthesisKey": synthesis_key(VIENEU_VOICE, 1.0, 0),
        "inputHash": "b" * 64, "sampleSetSha256": "c" * 64,
        "reviewer": "synthetic unit fixture (not human release evidence)",
        "reviewedAt": "2026-10-24T00:00:00Z",
        "amountAccuracy": {"correct": 57, "total": 57},
        "reviews": [{"clipId": "amount-1000", "sha256": "a" * 64, "approved": True,
                     "readability": 4, "naturalness": 4, "clipping": False,
                     "missingWords": False, "repeatedWords": False}],
    }


@pytest.mark.parametrize("accuracy", [None, {}, {"correct": None, "total": None}, {"correct": 49, "total": 49},
                                      {"correct": 49, "total": 50}, {"correct": 50.0, "total": 50},
                                      {"correct": True, "total": 50}, {"correct": 50, "total": "50"},
                                      {"correct": 50, "total": 50, "automatic": True}])
def test_pack_review_requires_explicit_integer_human_accuracy(tmp_path, accuracy):
    review = synthetic_pack_review()
    review["amountAccuracy"] = accuracy
    path = write_json(tmp_path / "review.json", review)
    output = tmp_path / "quality.json"
    with pytest.raises(ValueError, match="Explicit human amount review"):
        quality.run_validate_pack_review(SimpleNamespace(review=path, quality_output=output))
    assert not output.exists()


def test_pack_review_preserves_actual_reviewed_amount_counts(tmp_path):
    review = synthetic_pack_review()
    path = write_json(tmp_path / "review.json", review)
    output = tmp_path / "quality.json"
    quality.run_validate_pack_review(SimpleNamespace(review=path, quality_output=output))
    evidence = json.loads(output.read_text(encoding="utf-8"))
    assert evidence["amountAccuracy"] == {"correct": 57, "total": 57}
    assert evidence["selectedRate"] == 1.0
    assert evidence["tailMode"] == evidence["numberMode"] == "whole"
    assert evidence["heardRecipePairIds"] == []
    assert evidence["candidateDecisions"] == {"tail": "rejected", "numbers": "rejected"}
    assert evidence["heardClipIds"] == ["amount-1000"]


def test_benchmark_posts_fixed_vieneu_with_eight_second_budget(tmp_path, monkeypatch):
    import urllib.request

    corpus = write_json(tmp_path / "corpus.json", {"cases": [{"id": "amount-1000", "kind": "amount", "transcript": "Đã nhận một nghìn đồng."}]})
    token = tmp_path / "token"
    token.write_text("synthetic-test-token", encoding="utf-8")
    requests = []

    class SyntheticResponse:
        status = 200

        def __init__(self):
            self.chunks = iter([b"synthetic transport fixture", b""])

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self, size):
            return next(self.chunks)

    def capture(request, *, timeout):
        requests.append(request)
        assert timeout == 10
        return SyntheticResponse()

    monkeypatch.setattr(urllib.request, "urlopen", capture)
    output = tmp_path / "benchmark"
    quality.run_benchmark(SimpleNamespace(base_url="http://fixture.invalid/", token_file=token, corpus=corpus, output=output, repeats=1, concurrency=1))
    assert len(requests) == 1
    assert requests[0].full_url == "http://fixture.invalid/synthesize/stream"
    assert json.loads(requests[0].data) == {
        "text": "Đã nhận một nghìn đồng.", "voice": VIENEU_VOICE, "provider_mode": "VIENEU_ONLY",
        "rate": "+0%", "pitch": "+0Hz", "cacheable": False, "timeout_ms": 8000,
    }
    assert requests[0].get_header("X-internal-tts-token") == "synthetic-test-token"
    results = json.loads((output / "benchmark-c1.json").read_text(encoding="utf-8"))
    assert len(results) == 1
    assert results[0]["status"] == 200 and results[0]["error"] is None


@pytest.fixture
def synthetic_listening_review(tmp_path):
    audio = tmp_path / "audio"
    audio.mkdir()
    items = []
    reviews = []
    for index, amount in enumerate(quality.CANONICAL_AMOUNTS_50):
        test_id = f"blind-{index + 1:03d}"
        raw = f"synthetic unit fixture {amount}".encode("utf-8")
        (audio / f"{test_id}.mp3").write_bytes(raw)
        items.append({"testId": test_id, "kind": "amount", "amountVnd": amount, "rate": 1.0,
                      "audioFile": f"audio/{test_id}.mp3", "audioSha256": hashlib.sha256(raw).hexdigest()})
        reviews.append({"testId": test_id, "heardAmount": amount, "readability": 4, "naturalness": 4,
                        "clipping": False, "abruptJoin": False, "missingWords": False, "repeatedWords": False})
    mapping = {"schemaVersion": 2, "providerMode": "VIENEU_ONLY", "voiceId": VIENEU_VOICE,
               "rates": [1.0], "items": items}
    review = {"reviewer": "synthetic fixture, not human release approval", "reviewedAt": "2026-10-24T00:00:00Z",
              "reviews": reviews, "overrideRate": None}
    return review, mapping


def test_validate_writes_only_fixed_vieneu_profile(tmp_path, synthetic_listening_review):
    review, mapping = synthetic_listening_review
    write_json(tmp_path / "mapping.sealed.json", mapping)
    review_path = write_json(tmp_path / "review.json", review)
    output = tmp_path / "profile.json"
    quality.run_validate(SimpleNamespace(review=review_path, profile_output=output))
    assert json.loads(output.read_text(encoding="utf-8")) == {
        "providerMode": "VIENEU_ONLY", "voiceId": VIENEU_VOICE, "rate": 1.0,
        "pitchHz": 0, "catalogVersion": "v1",
    }


@pytest.mark.parametrize("damage", ["old-schema", "old-provider", "old-voice", "old-rate", "item-rate"])
def test_validate_rejects_nonfixed_sealed_profiles(tmp_path, synthetic_listening_review, damage):
    review, mapping = synthetic_listening_review
    if damage == "old-schema":
        mapping["schemaVersion"] = 1
    elif damage == "old-provider":
        mapping["providerMode"] = "EDGE_ONLY"
    elif damage == "old-voice":
        mapping["voiceId"] = "vi-VN-HoaiMyNeural"
    elif damage == "old-rate":
        mapping["rates"] = [1.25]
    else:
        mapping["items"][0]["rate"] = 1.25
    write_json(tmp_path / "mapping.sealed.json", mapping)
    review_path = write_json(tmp_path / "review.json", review)
    output = tmp_path / "profile.json"
    with pytest.raises(ValueError, match="fixed VieNeu profile"):
        quality.run_validate(SimpleNamespace(review=review_path, profile_output=output))
    assert not output.exists()


def test_validate_refuses_changed_audio(tmp_path, synthetic_listening_review):
    review, mapping = synthetic_listening_review
    write_json(tmp_path / "mapping.sealed.json", mapping)
    review_path = write_json(tmp_path / "review.json", review)
    (tmp_path / mapping["items"][0]["audioFile"]).write_bytes(b"tampered fixture")
    output = tmp_path / "profile.json"
    with pytest.raises(ValueError, match="Audio hash mismatch"):
        quality.run_validate(SimpleNamespace(review=review_path, profile_output=output))
    assert not output.exists()


def test_validate_cannot_override_a_wrong_heard_amount(tmp_path, synthetic_listening_review):
    review, mapping = synthetic_listening_review
    review["reviews"][0]["heardAmount"] = "999"
    review["overrideRate"] = 1.0
    write_json(tmp_path / "mapping.sealed.json", mapping)
    review_path = write_json(tmp_path / "review.json", review)
    output = tmp_path / "profile.json"
    with pytest.raises(SystemExit) as error:
        quality.run_validate(SimpleNamespace(review=review_path, profile_output=output))
    assert error.value.code == 3
    assert not output.exists()
