#!/usr/bin/env python3
"""Fetch and verify pinned VieNeu v3 Turbo ONNX assets offline.

Manifest: tts-gateway/assets/models/vieneu/provenance.json
Voice identity: vi-VN-vieneu-v3-turbo-mai-anh-v1
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import urllib.request

MANIFEST_PATH = Path(__file__).resolve().parents[1] / "assets" / "models" / "vieneu" / "provenance.json"

VOICE_ID = "vi-VN-vieneu-v3-turbo-mai-anh-v1"
MAI_ANH_PRESET_NAME = "Mai Anh"


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        while chunk := f.read(1024 * 1024):
            h.update(chunk)
    return h.hexdigest()


def download_file(url: str, dest: Path, expected_sha256: str | None = None, expected_bytes: int | None = None) -> tuple[str, int]:
    dest.parent.mkdir(parents=True, exist_ok=True)
    temp_dest = dest.with_name(f"{dest.name}.tmp.{os.getpid()}")
    h = hashlib.sha256()
    total_bytes = 0

    req = urllib.request.Request(url, headers={"User-Agent": "tbg-vieneu-fetch/1.0"})
    try:
        with urllib.request.urlopen(req) as resp, temp_dest.open("wb") as out:
            while chunk := resp.read(1024 * 1024):
                h.update(chunk)
                total_bytes += len(chunk)
                out.write(chunk)
    except Exception:
        if temp_dest.exists():
            temp_dest.unlink()
        raise

    calculated_hash = h.hexdigest()
    if expected_sha256 and calculated_hash != expected_sha256:
        if temp_dest.exists():
            temp_dest.unlink()
        raise ValueError(f"Hash mismatch for {dest.name}: got {calculated_hash}, expected {expected_sha256}")
    if expected_bytes is not None and total_bytes != expected_bytes:
        if temp_dest.exists():
            temp_dest.unlink()
        raise ValueError(f"Size mismatch for {dest.name}: got {total_bytes}, expected {expected_bytes}")

    temp_dest.replace(dest)
    return calculated_hash, total_bytes


def export_mai_anh_preset(dest: Path) -> tuple[str, int]:
    import pkgutil
    data = pkgutil.get_data("vieneu", "assets/voices_v3_turbo.json")
    if not data:
        raise RuntimeError("vieneu/assets/voices_v3_turbo.json not found in package")
    parsed = json.loads(data.decode("utf-8"))
    presets = parsed.get("presets", {})
    if MAI_ANH_PRESET_NAME not in presets:
        raise RuntimeError(f"Preset '{MAI_ANH_PRESET_NAME}' missing from package voices_v3_turbo.json")

    mai_anh_data = presets[MAI_ANH_PRESET_NAME]
    if mai_anh_data.get("gender") != "female" or mai_anh_data.get("region") != "Bắc":
        raise ValueError(f"Preset '{MAI_ANH_PRESET_NAME}' metadata mismatch: {mai_anh_data.get('gender')}, {mai_anh_data.get('region')}")

    out_content = {
        "meta": parsed.get("meta", {}),
        "default_voice": MAI_ANH_PRESET_NAME,
        "presets": {
            MAI_ANH_PRESET_NAME: mai_anh_data
        }
    }
    encoded = json.dumps(out_content, indent=2, ensure_ascii=False).encode("utf-8")
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_bytes(encoded)
    return hashlib.sha256(encoded).hexdigest(), len(encoded)


def verify_manifest(manifest: dict, out_dir: Path) -> None:
    errors = []
    # Check backbone files
    backbone_dir = out_dir / "backbone"
    for rel_path, meta in manifest["backbone"]["files"].items():
        fp = backbone_dir / rel_path
        if not fp.is_file():
            errors.append(f"Missing backbone file: {rel_path}")
            continue
        sz = fp.stat().st_size
        if sz != meta["bytes"]:
            errors.append(f"Size mismatch {rel_path}: {sz} vs {meta['bytes']}")
            continue
        h = sha256_file(fp)
        if h != meta["sha256"]:
            errors.append(f"Hash mismatch {rel_path}: {h} vs {meta['sha256']}")

    # Check preset
    preset_fp = backbone_dir / "voices_v3_turbo.json"
    if not preset_fp.is_file():
        errors.append("Missing preset file voices_v3_turbo.json")
    else:
        preset_meta = manifest["preset"]["files"]["voices_v3_turbo.json"]
        if preset_fp.stat().st_size != preset_meta["bytes"]:
            errors.append(f"Preset size mismatch: {preset_fp.stat().st_size} vs {preset_meta['bytes']}")
        elif sha256_file(preset_fp) != preset_meta["sha256"]:
            errors.append("Preset hash mismatch")
        else:
            p_data = json.loads(preset_fp.read_text(encoding="utf-8"))
            if MAI_ANH_PRESET_NAME not in p_data.get("presets", {}):
                errors.append("Preset Mai Anh not found in voices_v3_turbo.json")

    # Check codec snapshot and HF refs
    codec_snapshot_dir = out_dir / "hf" / "hub" / f"models--{manifest['codec']['repo'].replace('/', '--')}" / "snapshots" / manifest["codec"]["revision"]
    for rel_path, meta in manifest["codec"]["files"].items():
        fp = codec_snapshot_dir / rel_path
        if not fp.is_file():
            errors.append(f"Missing codec snapshot file: {rel_path}")
            continue
        sz = fp.stat().st_size
        if sz != meta["bytes"]:
            errors.append(f"Codec size mismatch {rel_path}: {sz} vs {meta['bytes']}")
            continue
        h = sha256_file(fp)
        if h != meta["sha256"]:
            errors.append(f"Codec hash mismatch {rel_path}: {h} vs {meta['sha256']}")

    # Check ref
    ref_fp = out_dir / "hf" / "hub" / f"models--{manifest['codec']['repo'].replace('/', '--')}" / "refs" / "main"
    if not ref_fp.is_file():
        errors.append("Missing HF refs/main for codec repo")
    else:
        ref_rev = ref_fp.read_text(encoding="utf-8").strip()
        if ref_rev != manifest["codec"]["revision"]:
            errors.append(f"HF refs/main points to {ref_rev} instead of {manifest['codec']['revision']}")

    if errors:
        raise RuntimeError("Verification failed:\n  " + "\n  ".join(errors))


def fetch_all(manifest: dict, out_dir: Path, update_manifest: bool = False) -> dict:
    backbone_dir = out_dir / "backbone"
    codec_repo_dir = out_dir / "hf" / "hub" / f"models--{manifest['codec']['repo'].replace('/', '--')}"
    codec_snapshot_dir = codec_repo_dir / "snapshots" / manifest["codec"]["revision"]

    backbone_dir.mkdir(parents=True, exist_ok=True)
    codec_snapshot_dir.mkdir(parents=True, exist_ok=True)

    # 1. Fetch backbone files
    bb_repo = manifest["backbone"]["repo"]
    bb_rev = manifest["backbone"]["revision"]
    for rel_path in manifest["backbone"]["file_list"]:
        dest = backbone_dir / rel_path
        url = f"https://huggingface.co/{bb_repo}/resolve/{bb_rev}/{rel_path}"
        expected_meta = manifest["backbone"].get("files", {}).get(rel_path) if not update_manifest else None
        expected_hash = expected_meta["sha256"] if expected_meta else None
        expected_bytes = expected_meta["bytes"] if expected_meta else None

        if dest.is_file() and expected_hash:
            if dest.stat().st_size == expected_bytes and sha256_file(dest) == expected_hash:
                print(f"[skip] backbone {rel_path} already present and verified")
                continue

        print(f"Downloading backbone {rel_path}...")
        h, sz = download_file(url, dest, expected_hash, expected_bytes)
        if update_manifest:
            manifest["backbone"].setdefault("files", {})[rel_path] = {
                "source": url,
                "sha256": h,
                "bytes": sz
            }

    # 2. Preset
    preset_dest = backbone_dir / "voices_v3_turbo.json"
    print("Extracting Mai Anh preset from vieneu wheel...")
    ph, psz = export_mai_anh_preset(preset_dest)
    if update_manifest:
        manifest.setdefault("preset", {})["voiceId"] = VOICE_ID
        manifest["preset"]["name"] = MAI_ANH_PRESET_NAME
        manifest["preset"]["gender"] = "female"
        manifest["preset"]["region"] = "Bắc"
        manifest["preset"]["files"] = {
            "voices_v3_turbo.json": {
                "sha256": ph,
                "bytes": psz
            }
        }

    # 3. Codec files
    codec_repo = manifest["codec"]["repo"]
    codec_rev = manifest["codec"]["revision"]
    for rel_path in manifest["codec"]["file_list"]:
        dest = codec_snapshot_dir / rel_path
        url = f"https://huggingface.co/{codec_repo}/resolve/{codec_rev}/{rel_path}"
        expected_hash = expected_meta["sha256"] if expected_meta else None
        expected_bytes = expected_meta["bytes"] if expected_meta else None

        if dest.is_file() and expected_hash:
            if dest.stat().st_size == expected_bytes and sha256_file(dest) == expected_hash:
                print(f"[skip] codec {rel_path} already present and verified")
                continue

        print(f"Downloading codec {rel_path}...")
        ch, csz = download_file(url, dest, expected_hash, expected_bytes)
        if update_manifest:
            manifest["codec"].setdefault("files", {})[rel_path] = {
                "source": url,
                "sha256": ch,
                "bytes": csz
            }

    # 4. Write refs/main
    refs_dir = codec_repo_dir / "refs"
    refs_dir.mkdir(parents=True, exist_ok=True)
    (refs_dir / "main").write_text(codec_rev, encoding="utf-8")

    return manifest


def main():
    parser = argparse.ArgumentParser(description="Fetch and verify VieNeu v3 Turbo assets offline")
    parser.add_argument("--output", type=str, required=True, help="Destination directory for model assets")
    parser.add_argument("--verify-only", action="store_true", help="Only verify existing files against manifest without downloading")
    parser.add_argument("--update-manifest", action="store_true", help="Recalculate hashes and update manifest (internal use)")
    args = parser.parse_args()

    out_dir = Path(args.output).resolve()

    if args.verify_only:
        if not MANIFEST_PATH.exists():
            raise FileNotFoundError(f"Manifest not found: {MANIFEST_PATH}")
        with MANIFEST_PATH.open("r", encoding="utf-8") as f:
            manifest = json.load(f)
        print(f"Verifying {out_dir} against {MANIFEST_PATH}...")
        verify_manifest(manifest, out_dir)
        print("Verification PASSED: all files present and hashes match.")
        return

    if args.update_manifest or not MANIFEST_PATH.exists():
        manifest = {
            "schemaVersion": 1,
            "runtime": "vieneu==3.8.3",
            "voiceId": VOICE_ID,
            "backbone": {
                "repo": "pnnbao-ump/VieNeu-TTS-v3-Turbo",
                "revision": "61b85e3d937fbbacb387714180e8182823512523",
                "file_list": [
                    "onnx_update/config.json",
                    "onnx_update/tokenizer.json",
                    "onnx_update/vieneu_prefill.onnx",
                    "onnx_update/vieneu_decode_step.onnx",
                    "onnx_update/vieneu_acoustic_cached.onnx",
                    "onnx_update/vieneu_backbone_shared.data",
                    "onnx_update/vieneu_v3_heads.npz",
                    "config.json",
                    "README.md"
                ],
                "files": {}
            },
            "codec": {
                "repo": "OpenMOSS-Team/MOSS-Audio-Tokenizer-Nano-ONNX",
                "revision": "ceff0d0749bfb3fa2d61149794ec6feef0d1e1ae",
                "file_list": [
                    "moss_audio_tokenizer_decode_full.onnx",
                    "moss_audio_tokenizer_decode_shared.data",
                    "moss_audio_tokenizer_decode_step.onnx",
                    "codec_browser_onnx_meta.json",
                    "moss_audio_tokenizer_encode.onnx",
                    "moss_audio_tokenizer_encode.data",
                    "README.md"
                ],
                "files": {}
            }
        }
        updated_manifest = fetch_all(manifest, out_dir, update_manifest=True)
        MANIFEST_PATH.parent.mkdir(parents=True, exist_ok=True)
        with MANIFEST_PATH.open("w", encoding="utf-8") as f:
            json.dump(updated_manifest, f, indent=2, ensure_ascii=False)
        print(f"Manifest written to {MANIFEST_PATH}")
    else:
        with MANIFEST_PATH.open("r", encoding="utf-8") as f:
            manifest = json.load(f)
        fetch_all(manifest, out_dir, update_manifest=False)

    verify_manifest(manifest, out_dir)
    print("Fetch and verification completed successfully.")


if __name__ == "__main__":
    main()
