#!/usr/bin/env python3
"""
release-state.py — Canonical release state schema v2 validator, builder, and reader.

Ensures the release state document is the authoritative source of truth:
- Enforces schema v2 constraints, immutable image digests (@sha256:64hex),
  resolved release bundle directories under RUNTIME_RELEASES_DIR, and matching manifest hashes.
- Builds deterministic candidate state directly from previous canonical state + signed manifest.
- Provides atomic query and .release.env compatibility projection generation.
"""

import argparse
import copy
import hashlib
import json
import os
import re
import sys
from pathlib import Path
from typing import Any, Dict, Optional

HEX40_RE = re.compile(r"^[0-9a-f]{40}$")
HEX64_RE = re.compile(r"^[0-9a-f]{64}$")
IMAGE_DIGEST_RE = re.compile(r"^[^\s]+@sha256:[0-9a-f]{64}$")
RELEASE_ID_RE = re.compile(r"^[A-Za-z0-9._-]{1,128}$")

REQUIRED_COMPONENTS = ["frontend", "gateway", "worker", "dbtool", "auth_browser", "tts", "bark"]


def die(msg: str) -> None:
    sys.stderr.write(f"release-state error: {msg}\n")
    sys.exit(1)


def validate_image_ref(ref: Any, name: str) -> None:
    if not isinstance(ref, str) or not IMAGE_DIGEST_RE.fullmatch(ref):
        die(f"Image for '{name}' must be immutable @sha256:64hex digest, got: {ref}")


def validate_release_dir(path_str: str, expected_manifest_sha: Optional[str] = None, allow_candidate: bool = False) -> Path:
    p = Path(path_str)
    if not p.is_absolute():
        die(f"release_dir must be absolute: {path_str}")
    if p.is_symlink():
        die(f"release_dir must not be a symlink: {path_str}")
    
    releases_root_env = os.environ.get("RUNTIME_RELEASES_DIR")
    if releases_root_env:
        try:
            resolved_root = Path(releases_root_env).resolve()
            p.resolve().relative_to(resolved_root)
        except Exception:
            die(f"release_dir {path_str} must resolve under RUNTIME_RELEASES_DIR ({releases_root_env})")

    manifest_path = p / "release-manifest.json"
    if not manifest_path.is_file():
        if not allow_candidate:
            die(f"Missing release-manifest.json in release_dir: {path_str}")
        return p

    if expected_manifest_sha:
        content = manifest_path.read_bytes()
        actual_sha = hashlib.sha256(content).hexdigest()
        if actual_sha.lower() != expected_manifest_sha.lower():
            die(f"manifest_sha256 mismatch in {manifest_path}: expected {expected_manifest_sha}, got {actual_sha}")

    return p


def validate_state(state: Dict[str, Any], allow_candidate: bool = False) -> None:
    if not isinstance(state, dict):
        die("State must be a JSON object")

    ver = state.get("schema_version")
    if ver != 2:
        die(f"Unsupported schema_version: {ver} (expected 2)")

    gen = state.get("generation")
    if not isinstance(gen, int) or gen < 1:
        die(f"Invalid generation: {gen}")

    rel_id = state.get("release_id", "")
    if not isinstance(rel_id, str) or not RELEASE_ID_RE.fullmatch(rel_id):
        die(f"Invalid release_id: {rel_id}")

    git_sha = state.get("git_sha", "")
    if not isinstance(git_sha, str) or not HEX40_RE.fullmatch(git_sha):
        die(f"Invalid git_sha: {git_sha}")

    manifest_sha = state.get("manifest_sha256", "")
    if not isinstance(manifest_sha, str) or not HEX64_RE.fullmatch(manifest_sha):
        die(f"Invalid manifest_sha256: {manifest_sha}")

    status = state.get("status", "")
    valid_statuses = ("COMPLETED", "PREPARED", "CANDIDATE") if allow_candidate else ("COMPLETED",)
    if status not in valid_statuses:
        die(f"Invalid status: {status} (allowed: {valid_statuses})")

    rel_dir = state.get("release_dir", "")
    if not rel_dir or not isinstance(rel_dir, str):
        die(f"release_dir is required")
    validate_release_dir(rel_dir, manifest_sha, allow_candidate=allow_candidate)

    # Previous release validation
    prev = state.get("previous")
    if prev is not None:
        if not isinstance(prev, dict):
            die("previous must be an object or null")
        p_gen = prev.get("generation")
        if not isinstance(p_gen, int) or p_gen >= gen:
            die(f"previous generation ({p_gen}) must be < current generation ({gen})")
        p_id = prev.get("release_id", "")
        if not isinstance(p_id, str) or not RELEASE_ID_RE.fullmatch(p_id):
            die(f"Invalid previous release_id: {p_id}")
        p_dir = prev.get("release_dir", "")
        if p_dir:
            validate_release_dir(p_dir, allow_candidate=True)

    # Active slots
    slots = state.get("active_slots", {})
    if not isinstance(slots, dict):
        die("active_slots must be an object")
    gw_slot = slots.get("gateway")
    if gw_slot not in ("blue", "green"):
        die(f"active_slots.gateway must be 'blue' or 'green', got: {gw_slot}")
    fe_slot = slots.get("frontend")
    if fe_slot not in ("blue", "green", "legacy"):
        die(f"active_slots.frontend must be 'blue', 'green', or 'legacy', got: {fe_slot}")

    # Images
    images = state.get("images", {})
    if not isinstance(images, dict):
        die("images must be an object")

    gw_imgs = images.get("gateway")
    if not isinstance(gw_imgs, dict):
        die("images.gateway must be an object with 'blue' and 'green' keys")
    validate_image_ref(gw_imgs.get("blue"), "images.gateway.blue")
    validate_image_ref(gw_imgs.get("green"), "images.gateway.green")

    for comp in ["frontend", "worker", "dbtool", "auth_browser", "tts", "bark"]:
        validate_image_ref(images.get(comp), f"images.{comp}")

    # Config / bundles hashes
    cfg = state.get("config")
    if cfg is not None:
        if not isinstance(cfg, dict):
            die("config must be an object")
        for k in ["compose_bundle_sha256", "traefik_template_sha256", "platform_bundle_sha256"]:
            val = cfg.get(k)
            if val is not None and val != "":
                if not isinstance(val, str) or not HEX64_RE.fullmatch(val):
                    die(f"Invalid config hash for {k}: {val}")

    fc = state.get("failover_controller")
    if fc is not None:
        if not isinstance(fc, dict):
            die("failover_controller must be an object")
        for k in ["sha256", "bundle_sha256"]:
            val = fc.get(k)
            if val is not None and val != "":
                if not isinstance(val, str) or not HEX64_RE.fullmatch(val):
                    die(f"Invalid failover_controller hash for {k}: {val}")


def get_nested(data: Any, path: str) -> Any:
    keys = path.split(".")
    curr = data
    for k in keys:
        if isinstance(curr, dict):
            curr = curr.get(k)
        elif isinstance(curr, list):
            try:
                curr = curr[int(k)]
            except (ValueError, IndexError):
                return None
        else:
            return None
    return curr


def build_candidate_state(
    prev_path: Optional[str],
    release_dir: str,
    manifest_path: str,
    candidate_gw_slot: str,
    candidate_fe_slot: Optional[str],
    promoted_scope: Optional[str] = None,
) -> Dict[str, Any]:
    with open(manifest_path, "r", encoding="utf-8") as f:
        manifest_data = json.load(f)

    manifest_bytes = Path(manifest_path).read_bytes()
    manifest_sha = hashlib.sha256(manifest_bytes).hexdigest()

    prev_state: Dict[str, Any] = {}
    prev_gen = 0
    prev_id = None
    prev_dir = None

    if prev_path and os.path.isfile(prev_path):
        with open(prev_path, "r", encoding="utf-8") as f:
            prev_raw = json.load(f)
        if prev_raw.get("schema_version") == 2:
            prev_state = prev_raw
            prev_gen = int(prev_state.get("generation", 0))
            prev_id = prev_state.get("release_id")
            prev_dir = prev_state.get("release_dir")
        elif prev_raw.get("schema_version") == 1:
            prev_gen = int(prev_raw.get("generation", 0))
            prev_id = prev_raw.get("release_id")
            prev_state = {
                "schema_version": 2,
                "generation": prev_gen,
                "release_id": prev_id,
                "git_sha": prev_raw.get("git_sha", ""),
                "manifest_sha256": prev_raw.get("manifest_sha256", ""),
                "status": "COMPLETED",
                "active_slots": prev_raw.get("active_slots", {"gateway": "blue", "frontend": "legacy"}),
                "images": prev_raw.get("images", {}),
            }

    candidate_gen = prev_gen + 1
    rel_id = manifest_data.get("release_id")
    git_sha = manifest_data.get("git_sha")

    manifest_images = manifest_data.get("images", {})

    scope_set = set(promoted_scope.split(",")) if promoted_scope else set(REQUIRED_COMPONENTS + ["failover_controller"])

    candidate_images: Dict[str, Any] = copy.deepcopy(prev_state.get("images", {}))
    if not isinstance(candidate_images.get("gateway"), dict):
        candidate_images["gateway"] = {}

    # Update images based on promoted scope
    if "gateway" in scope_set or not candidate_images.get("gateway"):
        gw_img = manifest_images.get("gateway")
        if gw_img:
            candidate_images["gateway"][candidate_gw_slot] = gw_img
            # Keep standby image if existing
            if not candidate_images["gateway"].get("blue"):
                candidate_images["gateway"]["blue"] = gw_img
            if not candidate_images["gateway"].get("green"):
                candidate_images["gateway"]["green"] = gw_img

    for comp in ["frontend", "worker", "dbtool", "auth_browser", "tts", "bark"]:
        if comp in scope_set or comp not in candidate_images:
            if manifest_images.get(comp):
                candidate_images[comp] = manifest_images[comp]

    if not candidate_fe_slot:
        candidate_fe_slot = prev_state.get("active_slots", {}).get("frontend") or "legacy"

    candidate: Dict[str, Any] = {
        "schema_version": 2,
        "generation": candidate_gen,
        "release_id": rel_id,
        "release_dir": str(Path(release_dir).resolve()),
        "git_sha": git_sha,
        "manifest_sha256": manifest_sha,
        "status": "COMPLETED",
        "previous": {
            "generation": prev_gen,
            "release_id": prev_id,
            "release_dir": prev_dir,
        } if prev_id else None,
        "active_slots": {
            "gateway": candidate_gw_slot,
            "frontend": candidate_fe_slot,
        },
        "images": candidate_images,
    }

    # Record hashes if present
    artifacts = manifest_data.get("artifacts", {})
    cfg_entries = {}
    for k, art_key in [
        ("compose_bundle_sha256", "compose_bundle_sha256"),
        ("traefik_template_sha256", "lib/traefik.sh"),
        ("platform_bundle_sha256", "runtime-layout.sh"),
    ]:
        val = artifacts.get(art_key) or artifacts.get(art_key.replace("_sha256", ""))
        if not val and k == "compose_bundle_sha256":
            val = artifacts.get("compose.prod.yaml")
        if val:
            cfg_entries[k] = val
    if cfg_entries:
        candidate["config"] = cfg_entries

    fc_sha = artifacts.get("failover/vps-failover-controller.py", "")
    fc_bundle = artifacts.get("failover_bundle_sha256", "") or artifacts.get("failover-bundle", "")
    if fc_sha or fc_bundle:
        candidate["failover_controller"] = {
            "sha256": fc_sha or None,
            "bundle_sha256": fc_bundle or None,
        }

    return candidate


def advance_doc_only_state(
    prev_path: str,
    release_dir: str,
    manifest_path: str,
) -> Dict[str, Any]:
    with open(prev_path, "r", encoding="utf-8") as f:
        prev_raw = json.load(f)

    if prev_raw.get("schema_version") not in (1, 2) or prev_raw.get("status") != "COMPLETED":
        die("canonical release state is invalid")

    prev_gen = int(prev_raw.get("generation", 0))
    prev_id = prev_raw.get("release_id")
    prev_dir = prev_raw.get("release_dir")

    with open(manifest_path, "r", encoding="utf-8") as f:
        manifest_data = json.load(f)

    manifest_bytes = Path(manifest_path).read_bytes()
    manifest_sha = hashlib.sha256(manifest_bytes).hexdigest()

    rel_id = manifest_data.get("release_id") or f"rel-doc-{manifest_data.get('git_sha', '')}"
    git_sha = manifest_data.get("git_sha")

    active_slots = copy.deepcopy(prev_raw.get("active_slots", {"gateway": "blue", "frontend": "legacy"}))
    if not active_slots.get("frontend"):
        active_slots["frontend"] = "legacy"
    images = copy.deepcopy(prev_raw.get("images", {}))

    state: Dict[str, Any] = {
        "schema_version": 2,
        "generation": prev_gen + 1,
        "release_id": rel_id,
        "release_dir": str(Path(release_dir).resolve()),
        "git_sha": git_sha,
        "manifest_sha256": manifest_sha,
        "status": "COMPLETED",
        "previous": {
            "generation": prev_gen,
            "release_id": prev_id,
            "release_dir": prev_dir,
        } if prev_id else None,
        "active_slots": active_slots,
        "images": images,
    }

    if "config" in prev_raw:
        state["config"] = copy.deepcopy(prev_raw["config"])
    if "failover_controller" in prev_raw:
        state["failover_controller"] = copy.deepcopy(prev_raw["failover_controller"])

    validate_state(state, allow_candidate=False)
    return state


def export_release_env(state: Dict[str, Any], out_path: Optional[str] = None) -> str:
    images = state.get("images", {})
    gw_slot = state.get("active_slots", {}).get("gateway", "blue")
    gw_imgs = images.get("gateway", {})
    if isinstance(gw_imgs, dict):
        blue_img = gw_imgs.get("blue", "")
        green_img = gw_imgs.get("green", "")
    else:
        blue_img = str(gw_imgs)
        green_img = str(gw_imgs)

    lines = [
        "# Autogenerated projection from canonical release-state schema v2",
        f"RELEASE_ID={state.get('release_id', '')}",
        f"RELEASE_COMMIT={state.get('git_sha', '')}",
        f"CANONICAL_GENERATION={state.get('generation', 0)}",
        f"ACTIVE_GATEWAY_SLOT={gw_slot}",
        f"ACTIVE_FRONTEND_SLOT={state.get('active_slots', {}).get('frontend') or ''}",
        f"IMAGE_REF_BLUE={blue_img}",
        f"IMAGE_REF_GREEN={green_img}",
        f"FRONTEND_IMAGE_REF={images.get('frontend', '')}",
        f"WORKER_IMAGE_REF={images.get('worker', '')}",
        f"DBTOOL_IMAGE_REF={images.get('dbtool', '')}",
        f"BROWSER_IMAGE_REF={images.get('auth_browser', '')}",
        f"TTS_IMAGE_REF={images.get('tts', '')}",
        f"BARK_IMAGE_REF={images.get('bark', '')}",
    ]
    content = "\n".join(lines) + "\n"
    if out_path:
        with open(out_path, "w", encoding="utf-8") as f:
            f.write(content)
    return content


def main() -> None:
    parser = argparse.ArgumentParser(description="Canonical release state v2 management tool")
    sub = parser.add_subparsers(dest="cmd", required=True)

    # validate
    p_val = sub.add_parser("validate")
    p_val.add_argument("path", help="Path to release state JSON")
    p_val.add_argument("--allow-candidate", action="store_true", help="Allow CANDIDATE/PREPARED status")

    # get
    p_get = sub.add_parser("get")
    p_get.add_argument("path", help="Path to release state JSON")
    p_get.add_argument("key", help="Dotted key path (e.g. images.worker)")

    # build
    p_build = sub.add_parser("build")
    p_build.add_argument("--previous", help="Path to previous canonical state file")
    p_build.add_argument("--release-dir", required=True, help="Candidate release directory")
    p_build.add_argument("--manifest", required=True, help="Path to candidate release-manifest.json")
    p_build.add_argument("--gateway-slot", required=True, choices=["blue", "green"], help="Promoted gateway slot")
    p_build.add_argument("--frontend-slot", choices=["blue", "green", "legacy"], default=None, help="Promoted frontend slot")
    p_build.add_argument("--scope", default=None, help="Comma-separated promoted component scope")
    p_build.add_argument("--output", default=None, help="Output file path (default stdout)")

    # export-env
    p_env = sub.add_parser("export-env")
    p_env.add_argument("path", help="Path to release state JSON")
    p_env.add_argument("--output", default=None, help="Output file path (default stdout)")

    # advance-doc-only
    p_doc = sub.add_parser("advance-doc-only")
    p_doc.add_argument("--previous", required=True, help="Path to current canonical release state")
    p_doc.add_argument("--release-dir", required=True, help="Path to signed release directory")
    p_doc.add_argument("--manifest", required=True, help="Path to signed release manifest")
    p_doc.add_argument("--output", required=True, help="Output path for advanced release state")

    args = parser.parse_args()

    if args.cmd == "validate":
        with open(args.path, "r", encoding="utf-8") as f:
            state = json.load(f)
        validate_state(state, allow_candidate=args.allow_candidate)
        sys.exit(0)

    elif args.cmd == "get":
        with open(args.path, "r", encoding="utf-8") as f:
            state = json.load(f)
        val = get_nested(state, args.key)
        if val is None:
            sys.exit(1)
        if isinstance(val, (dict, list)):
            print(json.dumps(val, indent=2, sort_keys=True))
        else:
            print(val)
        sys.exit(0)

    elif args.cmd == "build":
        fe_slot = args.frontend_slot
        if fe_slot == "legacy":
            fe_slot = None
        state = build_candidate_state(
            prev_path=args.previous,
            release_dir=args.release_dir,
            manifest_path=args.manifest,
            candidate_gw_slot=args.gateway_slot,
            candidate_fe_slot=fe_slot,
            promoted_scope=args.scope,
        )
        content = json.dumps(state, indent=2, sort_keys=True) + "\n"
        if args.output:
            with open(args.output, "w", encoding="utf-8") as f:
                f.write(content)
        else:
            sys.stdout.write(content)
        sys.exit(0)

    elif args.cmd == "export-env":
        with open(args.path, "r", encoding="utf-8") as f:
            state = json.load(f)
        env_content = export_release_env(state, args.output)
        if not args.output:
            sys.stdout.write(env_content)
        sys.exit(0)

    elif args.cmd == "advance-doc-only":
        res = advance_doc_only_state(
            prev_path=args.previous,
            release_dir=args.release_dir,
            manifest_path=args.manifest,
        )
        content = json.dumps(res, indent=2, sort_keys=True) + "\n"
        with open(args.output, "w", encoding="utf-8") as f:
            f.write(content)
        sys.exit(0)


if __name__ == "__main__":
    main()
