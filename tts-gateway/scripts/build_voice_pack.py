#!/usr/bin/env python3
"""Build exact-profile Piper or explicitly authorized Edge packs.

Piper generation requires --model-card containing the fixed upstream CC-BY-4.0
model card. Its published license, not an Edge permission attestation, authorizes
the pack; release artifacts must retain model attribution. Edge generation still
requires --rights-evidence with reviewed deployment/use/retention/redistribution
permission. ffmpeg/ffprobe are build-only normalization dependencies.
"""
import argparse
import asyncio
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from app.assets import (
    HASH, MAX_CLIP_BYTES, MAX_MANIFEST_BYTES, MAX_PACK_BYTES,
    InvalidPack, canonical_json, parse_json, read_bounded, read_clip,
    manifest_inputs, read_quality_evidence, validate_quality_evidence,
    validate_clip_metadata, validate_inputs, validate_manifest,
)

BASE_APPROVED_AMOUNTS = {
    "1000", "2000", "5000", "10000", "15000", "20000", "25000", "30000",
    "40000", "50000", "75000", "100000", "150000", "200000", "300000",
    "500000", "750000", "1000000", "2000000", "5000000",
}


def load_inputs(path):
    data = validate_inputs(parse_json(read_bounded(Path(path), MAX_MANIFEST_BYTES)))
    actual_amounts = set(data["amounts"])
    if not (BASE_APPROVED_AMOUNTS <= actual_amounts):
        raise InvalidPack("Initial pack must contain at least the twenty base approved amounts")
    if len(actual_amounts) > 128:
        raise InvalidPack("Pack amounts cannot exceed 128")
    if any(set(mapping["classic"]) != {f"classic-{i:03d}" for i in range(1, 7)} for mapping in data["amounts"].values()):
        raise InvalidPack("Initial pack must contain all six classic phrases per amount")
    is_vieneu = data.get("voiceId") == "vi-VN-vieneu-v3-turbo-mai-anh-v1"
    if not is_vieneu:
        if sum(clip["kind"] == "tail-fragment" for clip in data["clips"]) != 600:
            raise InvalidPack("Initial pack must contain all 600 catalog fragments")
    else:
        if any(clip["kind"] in ("tail-fragment", "amount-segment") for clip in data["clips"]):
            raise InvalidPack("VieNeu pack must contain only whole utterances (no tail-fragment or amount-segment)")
        if len(data["clips"]) != len(actual_amounts) * 7:
            raise InvalidPack("VieNeu pack must contain exactly 7 whole clips per amount (1 standard + 6 classic)")
    return data

def require_rights(path):
    if path is None:
        raise InvalidPack("Audio rights gate: --rights-evidence required before synthesis")
    evidence = parse_json(read_bounded(Path(path), 64 * 1024))
    if not isinstance(evidence, dict) or evidence.get("authorized") is not True:
        raise InvalidPack("Audio rights gate: release owner must explicitly authorize generation")
    for field in ("deployment", "use", "retention", "redistribution", "evidenceReference"):
        if not isinstance(evidence.get(field), str) or not evidence[field].strip():
            raise InvalidPack("Audio rights gate: missing reviewed " + field + " evidence")


def require_piper_license(path):
    if path is None:
        raise InvalidPack("Piper license gate: --model-card required")
    card = read_bounded(Path(path), 64 * 1024)
    if hashlib.sha256(card).hexdigest() != "302db8a930ffc2b1c2181db26deaf8272116eca2b08b8f80da6375b0a994af7b":
        raise InvalidPack("Piper model card differs from pinned upstream card")
    if b"https://creativecommons.org/licenses/by/4.0/" not in card or b"vais1000" not in card.lower():
        raise InvalidPack("Piper model card does not grant the fixed voice license")


def require_vieneu_license(path):
    if path is None:
        raise InvalidPack("VieNeu license gate: --model-card required")
    card = read_bounded(Path(path), 64 * 1024)
    prov_path = Path(__file__).resolve().parents[1] / "assets" / "models" / "vieneu" / "provenance.json"
    if not prov_path.exists():
        raise InvalidPack("VieNeu provenance.json missing")
    with prov_path.open("r", encoding="utf-8") as f:
        prov = json.load(f)
    expected_hash = prov["backbone"]["files"]["README.md"]["sha256"]
    if hashlib.sha256(card).hexdigest() != expected_hash:
        raise InvalidPack("VieNeu model card differs from pinned upstream README.md")
    if b"VieNeu-TTS" not in card:
        raise InvalidPack("VieNeu model card does not match VieNeu-TTS")
def run_media(arguments):
    try:
        result = subprocess.run(arguments, capture_output=True, text=True, check=False, timeout=120)
    except subprocess.TimeoutExpired as exc:
        raise InvalidPack("Media processing exceeded 120-second build budget") from exc
    if result.returncode:
        raise InvalidPack("Media processing failed: " + result.stderr[-2000:])
    return result


def _measure_loudness(path, filter_chain):
    result = run_media(["ffmpeg", "-nostdin", "-hide_banner", "-i", str(path), "-af", filter_chain, "-f", "null", "-"])
    start = result.stderr.rfind("{")
    if start < 0:
        return None
    try:
        measured, _ = json.JSONDecoder().raw_decode(result.stderr[start:])
        for field in ("input_i", "input_tp", "input_lra", "input_thresh", "target_offset"):
            if not math.isfinite(float(measured[field])):
                return None
        return measured
    except (ValueError, TypeError, KeyError):
        return None


def loudness(path):
    measured = _measure_loudness(path, "loudnorm=I=-18:TP=-1.5:LRA=11:print_format=json")
    if measured is not None:
        return measured
    # For short speech clips (<1s), loudnorm returns -inf. Loop the speech
    # buffer to provide sufficient window for BS.1770 measurement without changing spectral content.
    measured = _measure_loudness(path, "aloop=loop=5:size=192000,loudnorm=I=-18:TP=-1.5:LRA=11:print_format=json")
    if measured is not None:
        return measured
    raise InvalidPack("Invalid loudness measurements: could not obtain finite loudness measurements")

def audio_metadata(path):
    result = run_media(["ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", str(path)])
    data = json.loads(result.stdout)
    streams = data.get("streams", [])
    if len(streams) != 1 or streams[0].get("codec_name") != "mp3" or streams[0].get("channels") != 1 or int(streams[0].get("sample_rate", 0)) != 24000:
        raise InvalidPack("Clip must be mono 24kHz MP3")
    duration = round(float(data["format"]["duration"]) * 1000)
    if not 0 < duration <= 120000:
        raise InvalidPack("Invalid clip duration")
    return duration


def normalize_audio(raw, temporary):
    source = temporary / "source.mp3"
    trimmed = temporary / "trimmed.wav"
    output = temporary / "normalized.mp3"
    source.write_bytes(raw)
    trim = "silenceremove=start_periods=1:start_duration=0.02:start_threshold=-50dB"
    filters = f"{trim},areverse,{trim},afade=t=in:d=0.005,areverse,afade=t=in:d=0.005"
    run_media(["ffmpeg", "-nostdin", "-y", "-v", "error", "-i", str(source), "-af", filters, "-ar", "24000", "-ac", "1", str(trimmed)])
    measured = loudness(trimmed)
    normalization = (
        "loudnorm=I=-18:TP=-1.5:LRA=11:linear=true"
        f":measured_I={measured['input_i']}:measured_TP={measured['input_tp']}"
        f":measured_LRA={measured['input_lra']}:measured_thresh={measured['input_thresh']}"
        f":offset={measured['target_offset']}"
    )
    run_media(["ffmpeg", "-nostdin", "-y", "-v", "error", "-i", str(trimmed), "-af", normalization, "-ar", "24000", "-ac", "1", "-codec:a", "libmp3lame", "-b:a", "64k", "-map_metadata", "-1", str(output)])
    actual = loudness(output)
    # Lossy encoding can change integrated loudness and inter-sample peaks.
    # Correct from the original PCM, never repeatedly transcode MP3. An
    # oversampled limiter provides codec headroom; every result is remeasured.
    gain_db = -18.0 - float(measured["input_i"])
    for _ in range(3):
        if -19 <= float(actual["input_i"]) <= -17 and float(actual["input_tp"]) <= -1:
            return read_bounded(output, MAX_CLIP_BYTES), audio_metadata(output)
        correction = f"volume={gain_db:.4f}dB,aresample=96000,alimiter=limit=0.749894:level=false:attack=5:release=50,aresample=24000"
        run_media(["ffmpeg", "-nostdin", "-y", "-v", "error", "-i", str(trimmed), "-af", correction, "-ar", "24000", "-ac", "1", "-codec:a", "libmp3lame", "-b:a", "64k", "-map_metadata", "-1", str(output)])
        actual = loudness(output)
        gain_db += -18.0 - float(actual["input_i"])
    if -19 <= float(actual["input_i"]) <= -17 and float(actual["input_tp"]) <= -1:
        return read_bounded(output, MAX_CLIP_BYTES), audio_metadata(output)
    artifact_root = os.environ.get("VOICE_PACK_FAILURE_ROOT")
    if artifact_root:
        destination = Path(artifact_root) / hashlib.sha256(raw).hexdigest()
        destination.mkdir(parents=True, exist_ok=True)
        for path in (source, trimmed, output):
            shutil.copyfile(path, destination / path.name)
        atomic_write(destination / "measurements.json", canonical_json({"source": measured, "encoded": actual}))
    raise InvalidPack(f"Normalized clip failed loudness/true-peak gate: I={actual['input_i']} LUFS, TP={actual['input_tp']} dBTP")


def atomic_write(path, content):
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as target:
        temporary = Path(target.name)
        target.write(content)
    try:
        temporary.replace(path)
    finally:
        temporary.unlink(missing_ok=True)


def verify_pack(root, directory, inspect_audio=True):
    raw = read_bounded(directory / "manifest.json", MAX_MANIFEST_BYTES)
    manifest = validate_manifest(parse_json(raw))
    if directory.name != manifest["synthesisKey"]:
        raise InvalidPack("Manifest/profile directory mismatch")
    read_quality_evidence(directory, manifest)
    for clip in manifest["clips"]:
        read_clip(root, clip)
        if inspect_audio and audio_metadata(root / "clips" / (clip["sha256"] + ".mp3")) != clip["durationMs"]:
            raise InvalidPack("Duration metadata mismatch")
    return manifest


def verify_output(root, inputs=None):
    if inputs is not None:
        directories = [root / inputs["synthesisKey"]]
    else:
        directories = sorted(path for path in root.iterdir() if HASH.fullmatch(path.name) and path.is_dir() and (path / "manifest.json").exists()) if root.is_dir() else []
    if not directories:
        raise InvalidPack("No published packs to verify")
    for directory in directories:
        manifest = verify_pack(root, directory)
        if inputs is not None:
            expected = {key: value for key, value in inputs.items() if key != "clips"}
            actual = {key: value for key, value in manifest.items() if key not in ("clips", "packVersion", "composition", "qualityEvidenceSha256")}
            transcripts = [{key: clip[key] for key in ("clipId", "kind", "transcript")} for clip in manifest["clips"]]
            if expected != actual or transcripts != inputs["clips"]:
                raise InvalidPack("Pack does not match exact input transcripts/profile")
        print(f"Verified {manifest['synthesisKey']}: {len(manifest['clips'])} clips")


async def build(inputs, root):
    from app.providers.piper import PiperTTSProvider, VOICE_ID as PIPER_VOICE_ID
    from app.providers.vieneu import VieneuTTSProvider, VOICE_ID as VIENEU_VOICE_ID
    is_piper = inputs["voiceId"] == PIPER_VOICE_ID
    is_vieneu = inputs["voiceId"] == VIENEU_VOICE_ID
    if is_piper:
        provider = PiperTTSProvider()
        concurrency = 2
    elif is_vieneu:
        provider = VieneuTTSProvider(threads=2)
        concurrency = 1
    else:
        from app.providers.edge import EdgeTTSProvider
        provider = EdgeTTSProvider()
        concurrency = 2
    directory = root / inputs["synthesisKey"]
    if (directory / "manifest.json").exists():
        verify_output(root, inputs)
        return
    directory.mkdir(parents=True, exist_ok=True)
    (root / "clips").mkdir(parents=True, exist_ok=True)
    checkpoint = directory / ".build-progress.json"
    input_hash = hashlib.sha256(canonical_json(inputs)).hexdigest()
    progress = {"inputHash": input_hash, "clips": {}}
    if checkpoint.exists():
        progress = parse_json(read_bounded(checkpoint, MAX_MANIFEST_BYTES))
        if progress.get("inputHash") != input_hash or not isinstance(progress.get("clips"), dict):
            raise InvalidPack("Resume inputs differ from this exact-profile build")
    completed = {}
    for clip in inputs["clips"]:
        saved = progress["clips"].get(clip["clipId"])
        if saved is not None:
            validate_clip_metadata(saved)
            if any(saved.get(key) != clip[key] for key in ("clipId", "kind", "transcript")):
                raise InvalidPack("Resume transcript mismatch")
            try:
                read_clip(root, saved)
            except InvalidPack:
                continue
            completed[clip["clipId"]] = saved
    semaphore = asyncio.Semaphore(concurrency)
    progress_lock = asyncio.Lock()

    async def create(clip):
        if clip["clipId"] in completed:
            return
        async with semaphore:
            # Preserve fixed voice/rate/pitch exactly; no fallback or retry.
            rate = f"{round((inputs['rate'] - 1) * 100):+d}%"
            pitch = f"{inputs['pitchHz']:+d}Hz"
            chunks = bytearray()
            async with asyncio.timeout(8):
                generator = provider.stream(clip["transcript"], inputs["voiceId"], rate, pitch)
                try:
                    async for chunk in generator:
                        if len(chunks) + len(chunk) > MAX_CLIP_BYTES:
                            raise InvalidPack("Upstream clip exceeds limit")
                        chunks.extend(chunk)
                finally:
                    await generator.aclose()
            with tempfile.TemporaryDirectory(prefix="voice-pack-") as temp:
                audio, duration = await asyncio.to_thread(normalize_audio, bytes(chunks), Path(temp))
            digest = hashlib.sha256(audio).hexdigest()
            metadata = dict(clip, durationMs=duration, sampleRate=24000, bytes=len(audio), sha256=digest, speechStartMs=None, amountStartMs=None)
            async with progress_lock:
                total = sum(item["bytes"] for item in completed.values()) + len(audio)
                if total > MAX_PACK_BYTES:
                    raise InvalidPack("Pack exceeds 64MiB; no clips were silently omitted")
                destination = root / "clips" / (digest + ".mp3")
                if destination.exists() and read_bounded(destination, MAX_CLIP_BYTES) != audio:
                    raise InvalidPack("Existing content-addressed clip is corrupt")
                if not destination.exists():
                    atomic_write(destination, audio)
                completed[clip["clipId"]] = metadata
                progress["clips"][clip["clipId"]] = metadata
                atomic_write(checkpoint, canonical_json(progress))

    # Two worker tasks, not a task per clip; resumable checkpoint on every success.
    pending = iter(inputs["clips"])

    async def worker():
        for clip in pending:
            try:
                await create(clip)
            except Exception as exc:
                raise InvalidPack(f"Clip {clip['clipId']} ({clip['transcript']!r}): {failure_details(exc)}") from exc

    try:
        if is_piper or is_vieneu:
            await provider.start()
        async with asyncio.TaskGroup() as group:
            for _ in range(concurrency):
                group.create_task(worker())
    finally:
        if is_piper or is_vieneu:
            import anyio
            with anyio.CancelScope(shield=True):
                await provider.close()
    manifest = dict(inputs, clips=[completed[clip["clipId"]] for clip in inputs["clips"]])
    manifest["composition"] = {"tail": "whole", "numbers": "whole"}
    manifest["packVersion"] = hashlib.sha256(canonical_json(manifest)).hexdigest()
    body = canonical_json(manifest)
    if len(body) > MAX_MANIFEST_BYTES:
        raise InvalidPack("Manifest exceeds 1MiB")
    atomic_write(directory / "manifest.candidate.json", body)
    checkpoint.unlink(missing_ok=True)
    print(f"Built candidate for {inputs['synthesisKey']}: {len(manifest['clips'])} clips; publish requires --publish and --quality-evidence")


def publish(inputs, root, quality_evidence_path):
    if not quality_evidence_path:
        raise InvalidPack("--quality-evidence required for --publish")
    validate_inputs(inputs)
    evidence = parse_json(read_bounded(quality_evidence_path, MAX_MANIFEST_BYTES))
    if not isinstance(evidence, dict):
        raise InvalidPack("Invalid quality evidence")
    evidence_hash = hashlib.sha256(canonical_json(evidence)).hexdigest()
    directory = root / inputs["synthesisKey"]
    if (directory / "manifest.json").exists():
        existing = verify_pack(root, directory)
        if manifest_inputs(existing) != inputs or existing["qualityEvidenceSha256"] != evidence_hash:
            raise InvalidPack("Published packs are immutable; use a separate output image")
        validate_quality_evidence(evidence, existing)
        return
    candidate = parse_json(read_bounded(directory / "manifest.candidate.json", MAX_MANIFEST_BYTES))
    if manifest_inputs(candidate) != inputs:
        raise InvalidPack("Candidate does not match exact input transcripts/profile")
    reviews = evidence.get("clips")
    if not isinstance(reviews, list) or len(reviews) != len(candidate["clips"]):
        raise InvalidPack("Quality clip review must cover the exact clip set")
    clips_evidence = {}
    for review in reviews:
        if not isinstance(review, dict) or not isinstance(review.get("clipId"), str) or review["clipId"] in clips_evidence:
            raise InvalidPack("Invalid/duplicate quality clip review")
        clips_evidence[review["clipId"]] = review
    published_clips = []
    for clip in candidate["clips"]:
        review = clips_evidence.get(clip["clipId"])
        if review is None or review.get("sha256") != clip["sha256"]:
            raise InvalidPack("Quality clip review must match every candidate hash")
        published_clips.append(dict(clip, speechStartMs=review.get("speechStartMs"), amountStartMs=review.get("amountStartMs")))
    manifest = dict(candidate, clips=published_clips)
    manifest["composition"] = {"tail": evidence.get("tailMode"), "numbers": evidence.get("numberMode")}
    manifest["qualityEvidenceSha256"] = evidence_hash
    unsigned = {k: v for k, v in manifest.items() if k != "packVersion"}
    manifest["packVersion"] = hashlib.sha256(canonical_json(unsigned)).hexdigest()
    validate_manifest(manifest)
    validate_quality_evidence(evidence, manifest)
    for clip in manifest["clips"]:
        read_clip(root, clip)
        if audio_metadata(root / "clips" / (clip["sha256"] + ".mp3")) != clip["durationMs"]:
            raise InvalidPack("Duration metadata mismatch")
    body = canonical_json(manifest)
    if len(body) > MAX_MANIFEST_BYTES:
        raise InvalidPack("Manifest exceeds 1MiB")
    atomic_write(directory / "quality.json", canonical_json(evidence))
    # The manifest is the commit point; candidates are never runtime-visible.
    atomic_write(directory / "manifest.json", body)
    print(f"Published {inputs['synthesisKey']} pack (version {manifest['packVersion'][:16]}...) with reviewed quality evidence")

def failure_details(error):
    if isinstance(error, BaseExceptionGroup):
        return "; ".join(failure_details(child) for child in error.exceptions)
    # Build diagnostics are local, never embedded in public manifests/responses.
    return f"{type(error).__name__}: {str(error)[:2000]}"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inputs", type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--verify-only", action="store_true")
    parser.add_argument("--publish", action="store_true")
    parser.add_argument("--quality-evidence", type=Path)
    parser.add_argument("--rights-evidence", type=Path)
    parser.add_argument("--model-card", type=Path)
    args = parser.parse_args()
    try:
        inputs = load_inputs(args.inputs) if args.inputs else None
        if args.verify_only:
            if not shutil.which("ffprobe"):
                raise InvalidPack("ffprobe required in build environment")
            verify_output(args.output, inputs)
        elif args.publish:
            if inputs is None:
                raise InvalidPack("--inputs required for --publish")
            if inputs["voiceId"] == "vi-VN-vais1000-medium":
                require_piper_license(args.model_card)
            elif inputs["voiceId"] == "vi-VN-vieneu-v3-turbo-mai-anh-v1":
                require_vieneu_license(args.model_card)
            else:
                require_rights(args.rights_evidence)
            if not shutil.which("ffprobe"):
                raise InvalidPack("ffprobe required in build environment")
            publish(inputs, args.output, args.quality_evidence)
        else:
            if inputs is None:
                raise InvalidPack("--inputs required for generation")
            if inputs["voiceId"] == "vi-VN-vais1000-medium":
                require_piper_license(args.model_card)
            elif inputs["voiceId"] == "vi-VN-vieneu-v3-turbo-mai-anh-v1":
                require_vieneu_license(args.model_card)
            else:
                require_rights(args.rights_evidence)
            if not shutil.which("ffmpeg") or not shutil.which("ffprobe"):
                raise InvalidPack("ffmpeg and ffprobe required in build environment")
            asyncio.run(build(inputs, args.output))
    except (InvalidPack, OSError, ValueError, RuntimeError, ExceptionGroup) as exc:
        parser.exit(1, f"Voice pack failed: {failure_details(exc)}\n")


if __name__ == "__main__":
    main()
