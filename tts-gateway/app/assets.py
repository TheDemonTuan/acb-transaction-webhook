"""Read-only, manifest-authorized voice packs. No runtime synthesis or writes."""
from dataclasses import dataclass
from decimal import Decimal
import hashlib
import json
import math
import os
from pathlib import Path
import re
import stat

MAX_MANIFEST_BYTES = 1024 * 1024
MAX_CLIP_BYTES = 4 * 1024 * 1024
MAX_PACK_BYTES = 64 * 1024 * 1024
FORMATTER_VERSION = "money-vi-v1"
PIPER_VOICE = "vi-VN-vais1000-medium"
VOICES = {"vi-VN-HoaiMyNeural", "vi-VN-NamMinhNeural", PIPER_VOICE}
HASH = re.compile(r"^[0-9a-f]{64}$")
ID = re.compile(r"^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$")
AMOUNT = re.compile(r"^[1-9][0-9]*$")
KINDS = {"amount", "classic", "tail-fragment", "tail-full"}


class InvalidPack(ValueError):
    pass


def canonical_json(value):
    # JSON has one number type: 1.00 must hash identically in Go, JS and Python.
    def normalize(item):
        if isinstance(item, dict):
            return {key: normalize(value) for key, value in item.items()}
        if isinstance(item, list):
            return [normalize(value) for value in item]
        if isinstance(item, float) and math.isfinite(item) and item.is_integer():
            return int(item)
        return item
    return json.dumps(normalize(value), ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise InvalidPack("Duplicate JSON key")
        result[key] = value
    return result


def parse_json(raw):
    try:
        return json.loads(raw, object_pairs_hook=_unique_object, parse_constant=lambda _: (_ for _ in ()).throw(InvalidPack("Non-finite JSON number")))
    except (ValueError, UnicodeError) as exc:
        raise InvalidPack("Invalid JSON") from exc


def _fields(value, expected):
    if not isinstance(value, dict) or set(value) != set(expected):
        raise InvalidPack("Invalid schema fields")


def synthesis_key(voice, rate, pitch):
    if not isinstance(voice, str) or voice not in VOICES or isinstance(rate, bool) or not isinstance(rate, (int, float)):
        raise InvalidPack("Unsupported voice/rate")
    if not math.isfinite(rate) or not 0.75 <= rate <= 2 or Decimal(str(rate)) * 100 != (Decimal(str(rate)) * 100).to_integral_value():
        raise InvalidPack("Invalid rate")
    if type(pitch) is not int or not -50 <= pitch <= 50:
        raise InvalidPack("Invalid pitch")
    provider = "piper" if voice == PIPER_VOICE else "edge"
    if provider == "piper" and pitch != 0:
        raise InvalidPack("Piper requires zero pitch")
    source = f"{provider}|{voice}|{rate:.2f}|{pitch}|{FORMATTER_VERSION}|pack-schema-1"
    return hashlib.sha256(source.encode("utf-8")).hexdigest()


def validate_inputs(data):
    _fields(data, ("schemaVersion", "synthesisKey", "voiceId", "rate", "pitchHz", "formatterVersion", "catalogVersion", "clips", "amounts"))
    _validate_common(data)
    ids = {}
    if not isinstance(data["clips"], list) or not data["clips"] or len(data["clips"]) > 20000:
        raise InvalidPack("Invalid clip count")
    for clip in data["clips"]:
        _fields(clip, ("clipId", "kind", "transcript"))
        _validate_clip_text(clip, ids)
    _validate_amounts(data["amounts"], ids)
    return data


def _validate_common(data):
    if type(data["schemaVersion"]) is not int or data["schemaVersion"] != 1 or data["formatterVersion"] != FORMATTER_VERSION or data["catalogVersion"] != "v1":
        raise InvalidPack("Unsupported pack version")
    if data["synthesisKey"] != synthesis_key(data["voiceId"], data["rate"], data["pitchHz"]):
        raise InvalidPack("Profile/key mismatch")


def _validate_clip_text(clip, ids):
    identifier = clip["clipId"]
    if not isinstance(identifier, str) or not ID.fullmatch(identifier) or identifier in ids:
        raise InvalidPack("Invalid/duplicate clip ID")
    if not isinstance(clip["kind"], str) or clip["kind"] not in KINDS or not isinstance(clip["transcript"], str) or not clip["transcript"].strip() or len(clip["transcript"]) > 1000:
        raise InvalidPack("Invalid transcript/kind")
    ids[identifier] = clip


def _validate_amounts(amounts, ids):
    if not isinstance(amounts, dict) or not amounts:
        raise InvalidPack("Missing amounts")
    for amount, mapping in amounts.items():
        if not AMOUNT.fullmatch(amount) or len(amount) > 16 or int(amount) > 9007199254740991:
            raise InvalidPack("Invalid canonical amount")
        _fields(mapping, ("standard", "classic"))
        standard = mapping["standard"]
        if not isinstance(standard, str) or standard not in ids or ids[standard]["kind"] != "amount":
            raise InvalidPack("Missing amount clip")
        if not isinstance(mapping["classic"], dict):
            raise InvalidPack("Invalid classic mapping")
        for phrase, identifier in mapping["classic"].items():
            if not ID.fullmatch(phrase) or not isinstance(identifier, str) or identifier not in ids or ids[identifier]["kind"] != "classic":
                raise InvalidPack("Missing classic clip")


def validate_clip_metadata(clip):
    _fields(clip, ("clipId", "kind", "transcript", "durationMs", "sampleRate", "bytes", "sha256"))
    _validate_clip_text(clip, {})
    if type(clip["bytes"]) is not int or not 0 < clip["bytes"] <= MAX_CLIP_BYTES:
        raise InvalidPack("Clip exceeds limit")
    if type(clip["durationMs"]) is not int or not 0 < clip["durationMs"] <= 120000 or type(clip["sampleRate"]) is not int or clip["sampleRate"] != 24000:
        raise InvalidPack("Invalid audio metadata")
    if not isinstance(clip["sha256"], str) or not HASH.fullmatch(clip["sha256"]):
        raise InvalidPack("Invalid clip hash")


def validate_manifest(data):
    _fields(data, ("schemaVersion", "packVersion", "synthesisKey", "voiceId", "rate", "pitchHz", "formatterVersion", "catalogVersion", "clips", "amounts"))
    _validate_common(data)
    version = data["packVersion"]
    unsigned = {k: v for k, v in data.items() if k != "packVersion"}
    if not isinstance(version, str) or not HASH.fullmatch(version) or hashlib.sha256(canonical_json(unsigned)).hexdigest() != version:
        raise InvalidPack("Manifest checksum mismatch")
    ids = {}
    total = 0
    if not isinstance(data["clips"], list) or not data["clips"] or len(data["clips"]) > 20000:
        raise InvalidPack("Invalid clip count")
    for clip in data["clips"]:
        validate_clip_metadata(clip)
        _validate_clip_text(clip, ids)
        total += clip["bytes"]
    if total > MAX_PACK_BYTES:
        raise InvalidPack("Pack exceeds limit")
    _validate_amounts(data["amounts"], ids)
    return data


def read_bounded(path, limit):
    # Reject symlinks at every published path component, including the root.
    if path.is_symlink() or any(parent.is_symlink() for parent in path.parents):
        raise InvalidPack("Symlink asset")
    try:
        if not stat.S_ISREG(path.stat().st_mode):
            raise InvalidPack("Asset is not a regular file")
        with path.open("rb") as source:
            metadata = os.fstat(source.fileno())
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_size > limit:
                raise InvalidPack("Asset exceeds limit")
            raw = source.read(limit + 1)
    except OSError as exc:
        raise InvalidPack("Asset missing/unreadable") from exc
    if len(raw) > limit:
        raise InvalidPack("Asset exceeds limit")
    return raw


def read_clip(root, clip):
    raw = read_bounded(root / "clips" / (clip["sha256"] + ".mp3"), MAX_CLIP_BYTES)
    if len(raw) != clip["bytes"] or hashlib.sha256(raw).hexdigest() != clip["sha256"]:
        raise InvalidPack("Clip checksum mismatch")
    return raw


@dataclass(frozen=True)
class PublishedPack:
    manifest: dict
    body: bytes


class AssetStore:
    def __init__(self, root):
        self.root = Path(root).absolute()
        self._packs = None

    def _load(self):
        if self._packs is not None:
            return
        packs = {}
        # Image content is immutable; only valid hash-named profile directories count.
        if self.root.is_dir() and not self.root.is_symlink():
            try:
                directories = list(self.root.iterdir())
            except OSError:
                directories = []
            for directory in directories:
                if not HASH.fullmatch(directory.name) or not directory.is_dir() or directory.is_symlink():
                    continue
                try:
                    raw = read_bounded(directory / "manifest.json", MAX_MANIFEST_BYTES)
                    manifest = validate_manifest(parse_json(raw))
                    if manifest["synthesisKey"] != directory.name:
                        raise InvalidPack("Profile directory mismatch")
                    for clip in manifest["clips"]:
                        read_clip(self.root, clip)
                    packs[directory.name] = PublishedPack(manifest, canonical_json(manifest))
                except (InvalidPack, TypeError, KeyError):
                    continue
        self._packs = packs

    def manifest(self, key):
        if not HASH.fullmatch(key):
            raise KeyError(key)
        self._load()
        return self._packs[key]

    def clip(self, digest):
        if not HASH.fullmatch(digest):
            raise KeyError(digest)
        self._load()
        for key, pack in list(self._packs.items()):
            for clip in pack.manifest["clips"]:
                if clip["sha256"] == digest:
                    try:
                        return read_clip(self.root, clip)
                    except InvalidPack:
                        del self._packs[key]
                        raise KeyError(digest) from None
        raise KeyError(digest)
