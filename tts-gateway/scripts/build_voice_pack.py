#!/usr/bin/env python3
"""Build approved exact-profile packs, or verify them without contacting Edge.

Network generation requires --rights-evidence: a release-owner JSON attestation
with authorized=true and nonempty deployment, use, retention, redistribution,
and evidenceReference strings. The attestation must refer to actual reviewed
permission; it is not itself a grant of rights. Keep private evidence out of packs.
"""
import argparse
import asyncio
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from app.assets import (
    HASH, MAX_CLIP_BYTES, MAX_MANIFEST_BYTES, MAX_PACK_BYTES,
    InvalidPack, canonical_json, parse_json, read_bounded, read_clip,
    validate_clip_metadata, validate_inputs, validate_manifest,
)

APPROVED_AMOUNTS = {
    "1000", "2000", "5000", "10000", "15000", "20000", "25000", "30000",
    "40000", "50000", "75000", "100000", "150000", "200000", "300000",
    "500000", "750000", "1000000", "2000000", "5000000",
}


def load_inputs(path):
    data = validate_inputs(parse_json(read_bounded(Path(path), MAX_MANIFEST_BYTES)))
    if set(data["amounts"]) != APPROVED_AMOUNTS:
        raise InvalidPack("Initial pack must contain the twenty approved amounts")
    if any(set(mapping["classic"]) != {f"classic-{i:03d}" for i in range(1, 7)} for mapping in data["amounts"].values()):
        raise InvalidPack("Initial pack must contain all six classic phrases per amount")
    if sum(clip["kind"] == "tail-fragment" for clip in data["clips"]) != 600:
        raise InvalidPack("Initial pack must contain all 600 catalog fragments")
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


def run_media(arguments):
    try:
        result = subprocess.run(arguments, capture_output=True, text=True, check=False, timeout=120)
    except subprocess.TimeoutExpired as exc:
        raise InvalidPack("Media processing exceeded 120-second build budget") from exc
    if result.returncode:
        raise InvalidPack("Media processing failed: " + result.stderr[-2000:])
    return result


def loudness(path):
    result = run_media(["ffmpeg", "-nostdin", "-hide_banner", "-i", str(path), "-af", "loudnorm=I=-18:TP=-1.5:LRA=11:print_format=json", "-f", "null", "-"])
    start = result.stderr.rfind("{")
    if start < 0:
        raise InvalidPack("Missing loudness measurements")
    try:
        return json.loads(result.stderr[start:])
    except ValueError as exc:
        raise InvalidPack("Invalid loudness measurements") from exc


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
    audio = read_bounded(output, MAX_CLIP_BYTES)
    duration = audio_metadata(output)
    actual = loudness(output)
    if not -19 <= float(actual["input_i"]) <= -17 or float(actual["input_tp"]) > -1:
        raise InvalidPack("Normalized clip failed loudness/true-peak gate")
    return audio, duration


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
    for clip in manifest["clips"]:
        read_clip(root, clip)
        if inspect_audio and audio_metadata(root / "clips" / (clip["sha256"] + ".mp3")) != clip["durationMs"]:
            raise InvalidPack("Duration metadata mismatch")
    return manifest


def verify_output(root, inputs=None):
    if inputs is not None:
        directories = [root / inputs["synthesisKey"]]
    else:
        directories = sorted(path for path in root.iterdir() if HASH.fullmatch(path.name) and path.is_dir()) if root.is_dir() else []
    if not directories:
        raise InvalidPack("No published packs to verify")
    for directory in directories:
        manifest = verify_pack(root, directory)
        if inputs is not None:
            expected = {key: value for key, value in inputs.items() if key != "clips"}
            actual = {key: value for key, value in manifest.items() if key not in ("clips", "packVersion")}
            transcripts = [{key: clip[key] for key in ("clipId", "kind", "transcript")} for clip in manifest["clips"]]
            if expected != actual or transcripts != inputs["clips"]:
                raise InvalidPack("Pack does not match exact input transcripts/profile")
        print(f"Verified {manifest['synthesisKey']}: {len(manifest['clips'])} clips")


async def build(inputs, root):
    # Imported only after the rights gate; verification never initializes Edge.
    from app.providers.edge import EdgeTTSProvider
    provider = EdgeTTSProvider()
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
    semaphore = asyncio.Semaphore(2)
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
            metadata = dict(clip, durationMs=duration, sampleRate=24000, bytes=len(audio), sha256=digest)
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
            await create(clip)

    async with asyncio.TaskGroup() as group:
        for _ in range(2):
            group.create_task(worker())
    manifest = dict(inputs, clips=[completed[clip["clipId"]] for clip in inputs["clips"]])
    manifest["packVersion"] = hashlib.sha256(canonical_json(manifest)).hexdigest()
    validate_manifest(manifest)
    body = canonical_json(manifest)
    if len(body) > MAX_MANIFEST_BYTES:
        raise InvalidPack("Manifest exceeds 1MiB")
    atomic_write(directory / "manifest.json", body)
    checkpoint.unlink(missing_ok=True)
    print(f"Built {inputs['synthesisKey']}: {len(manifest['clips'])} clips; listening/release gates still required")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inputs", type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--verify-only", action="store_true")
    parser.add_argument("--rights-evidence", type=Path)
    args = parser.parse_args()
    try:
        inputs = load_inputs(args.inputs) if args.inputs else None
        if args.verify_only:
            if not shutil.which("ffprobe"):
                raise InvalidPack("ffprobe required in build environment")
            verify_output(args.output, inputs)
        else:
            require_rights(args.rights_evidence)
            if inputs is None:
                raise InvalidPack("--inputs required for generation")
            if not shutil.which("ffmpeg") or not shutil.which("ffprobe"):
                raise InvalidPack("ffmpeg and ffprobe required in build environment")
            asyncio.run(build(inputs, args.output))
    except (InvalidPack, OSError, ValueError, ExceptionGroup) as exc:
        parser.exit(1, f"Voice pack failed: {exc}\n")


if __name__ == "__main__":
    main()
