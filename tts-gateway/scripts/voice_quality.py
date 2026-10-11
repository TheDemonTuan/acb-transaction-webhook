#!/usr/bin/env python3
"""Blind-listening preparation, rate validation, and comparison render runner."""
import argparse
import asyncio
import hashlib
import json
from datetime import datetime
import html
import math
from pathlib import Path
import random
import statistics
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from app.assets import canonical_json, manifest_inputs, read_clip, validate_inputs
from app.providers.piper import PiperTTSProvider, VOICE_ID

CANONICAL_AMOUNTS_50 = [
    "1", "5", "10", "15", "21", "24", "25", "101", "105", "1001", "10005", "49999", "50000",
    "99999", "100001", "199999", "200000", "999999", "1000000", "1000001", "1010005",
    "1000000000", "1000000000000", "9007199254740991", "1000", "2000", "5000", "10000",
    "15000", "20000", "25000", "30000", "40000", "75000", "100000", "105000", "127000",
    "150000", "201000", "215000", "251000", "300000", "368500", "500000", "750000",
    "1005000", "1015000", "2000000", "5000000", "1000000001",
]


async def run_prepare(args):
    corpus_path = Path(args.corpus)
    out_dir = Path(args.output)
    out_dir.mkdir(parents=True, exist_ok=True)
    audio_dir = out_dir / "audio"
    audio_dir.mkdir(parents=True, exist_ok=True)

    with corpus_path.open("r", encoding="utf-8") as f:
        corpus = json.load(f)

    if args.provider != "piper":
        raise ValueError("Only the actual Piper provider is supported; no relabeled audio")
    rates = [float(r.strip()) for r in args.rates.split(",") if r.strip()]
    if not rates or len(set(rates)) != len(rates) or any(not math.isfinite(r) or not 0.8 <= r <= 1.5 for r in rates):
        raise ValueError("Rates must be unique finite Piper rates in [0.8, 1.5]")
    amount_cases = [c for c in corpus["cases"] if c["kind"] == "amount"]
    if len(amount_cases) != 50 or {str(c.get("amountVnd")) for c in amount_cases} != set(CANONICAL_AMOUNTS_50):
        raise ValueError("Corpus must contain exactly the 50 distinct canonical amount cases")
    if len({c["id"] for c in amount_cases}) != 50:
        raise ValueError("Corpus case IDs must be unique")
    if (out_dir / "review.json").exists() or (out_dir / "mapping.sealed.json").exists():
        raise ValueError("Refusing to overwrite existing listening evidence; choose a fresh output directory")

    provider = PiperTTSProvider()
    await provider.start()

    tasks_to_generate = []
    for r in rates:
        for c in amount_cases:
            tasks_to_generate.append({
                "caseId": c["id"],
                "amountVnd": c.get("amountVnd", ""),
                "transcript": c["transcript"],
                "rate": r,
            })

    # Shuffle deterministically with seed 42
    rng = random.Random(42)
    rng.shuffle(tasks_to_generate)

    mapping = []
    blank_reviews = []

    print(f"Synthesizing {len(tasks_to_generate)} audio samples across rates {rates}...")
    try:
        for idx, item in enumerate(tasks_to_generate):
            test_id = f"blind-{idx+1:03d}"
            rate_str = f"{int(round((item['rate'] - 1) * 100)):+d}%"
            audio_bytes = await provider.synthesize(item["transcript"], voice=VOICE_ID, rate=rate_str)
            audio_file = audio_dir / f"{test_id}.mp3"
            audio_file.write_bytes(audio_bytes)
            audio_sha = hashlib.sha256(audio_bytes).hexdigest()

            mapping.append({
                "testId": test_id,
                "caseId": item["caseId"],
                "amountVnd": item["amountVnd"],
                "rate": item["rate"],
                "transcript": item["transcript"],
                "audioSha256": audio_sha,
                "audioFile": f"audio/{test_id}.mp3",
            })

            blank_reviews.append({
                "testId": test_id,
                "heardAmount": "",
                "readability": None,
                "naturalness": None,
                "abruptJoin": None,
                "clipping": None,
                "missingWords": None,
                "notes": "",
            })
    finally:
        await provider.close()

    # Save sealed mapping
    mapping_path = out_dir / "mapping.sealed.json"
    with mapping_path.open("w", encoding="utf-8") as f:
        json.dump({"schemaVersion": 1, "rates": rates, "items": mapping}, f, indent=2, ensure_ascii=False)

    # Save review template
    review_path = out_dir / "review.json"
    with review_path.open("w", encoding="utf-8") as f:
        json.dump({
            "schemaVersion": 1,
            "reviewer": "",
            "reviewedAt": "",
            "overrideRate": None,
            "reviews": blank_reviews,
        }, f, indent=2, ensure_ascii=False)

    # Generate minimal HTML player for offline listening
    html_content = f"""<!DOCTYPE html>
<html lang="vi">
<head>
<meta charset="utf-8"><title>TTS Blind Listening</title>
<style>
body {{ font-family: system-ui, sans-serif; max-width: 800px; margin: 2rem auto; padding: 0 1rem; color: #222; }}
.card {{ border: 1px solid #ddd; padding: 1rem; margin-bottom: 1rem; border-radius: 8px; background: #fafafa; }}
audio {{ width: 100%; margin-top: 0.5rem; }}
.grid {{ display: grid; grid-template-columns: 1fr 1fr; gap: 0.5rem; margin-top: 0.5rem; }}
label {{ font-size: 0.85rem; font-weight: 500; display: block; }}
input[type=text], input[type=number] {{ width: 100%; padding: 0.4rem; border: 1px solid #ccc; border-radius: 4px; box-sizing: border-box; }}
</style>
</head>
<body>
<h2>Bộ mẫu nghe mù chất lượng âm thanh (50 số tiền &times; {len(rates)} rates)</h2>
<p>Nghe từng mẫu, nhập số tiền nghe được (chỉ số), đánh giá độ dễ nghe (1-5) và độ tự nhiên (1-5).</p>
<div id="list">
{"".join(f'''<div class="card">
  <strong>{m["testId"]}</strong>
  <audio controls src="{m["audioFile"]}"></audio>
  <div class="grid">
    <div><label>Số tiền nghe được (VND):</label><input type="text" data-id="{m["testId"]}" class="heard"></div>
    <div><label>Dễ nghe (1-5):</label><input type="number" min="1" max="5" data-id="{m["testId"]}" class="readability"></div>
    <div><label>Tự nhiên (1-5):</label><input type="number" min="1" max="5" data-id="{m["testId"]}" class="naturalness"></div>
  </div>
</div>''' for m in mapping)}
</div>
</body>
</html>"""
    (out_dir / "player.html").write_text(html_content, encoding="utf-8")
    print(f"Prepared {len(mapping)} blind samples in {out_dir}.")
    print(f"Review template: {review_path}")
    print(f"Local player: {out_dir / 'player.html'}")


def run_validate(args):
    review_path = Path(args.review)
    profile_out = Path(args.profile_output)

    with review_path.open("r", encoding="utf-8") as f:
        review_data = json.load(f)

    if not isinstance(review_data.get("reviewer"), str) or not review_data["reviewer"].strip():
        raise ValueError("A named human reviewer is required")
    reviewed_at = datetime.fromisoformat(review_data.get("reviewedAt", "").replace("Z", "+00:00"))
    if reviewed_at.tzinfo is None:
        raise ValueError("reviewedAt must include a timezone")
    mapping_path = review_path.parent / "mapping.sealed.json"
    if not mapping_path.exists():
        raise FileNotFoundError(f"Sealed mapping not found at {mapping_path}")

    with mapping_path.open("r", encoding="utf-8") as f:
        sealed = json.load(f)

    mapping_by_id = {item["testId"]: item for item in sealed["items"]}
    if review_data.get("status") == "INVALID":
        raise ValueError("Withdrawn review is not human approval")
    reviews = review_data.get("reviews", [])
    if len(mapping_by_id) != len(sealed["items"]) or not sealed.get("rates") or len(set(sealed["rates"])) != len(sealed["rates"]):
        raise ValueError("Mapping must contain unique IDs and rates")
    review_ids = [rev.get("testId") for rev in reviews]
    if len(set(review_ids)) != len(review_ids) or set(review_ids) != set(mapping_by_id):
        raise ValueError("Reviews must cover every sample ID exactly once")
    for rate in sealed["rates"]:
        items = [m for m in sealed["items"] if m["rate"] == rate]
        if len(items) != 50 or {str(m.get("amountVnd")) for m in items} != set(CANONICAL_AMOUNTS_50):
            raise ValueError("Each rate requires the 50 distinct canonical amounts")
    for item in sealed["items"]:
        audio_path = (review_path.parent / item["audioFile"]).resolve()
        if not audio_path.is_relative_to(review_path.parent.resolve()):
            raise ValueError("Audio sample must remain within the listening directory")
        if hashlib.sha256(audio_path.read_bytes()).hexdigest() != item["audioSha256"]:
            raise ValueError(f"Audio hash mismatch for {item['testId']}")

    if len(reviews) != len(sealed["items"]):
        print(f"Validation failed: expected {len(sealed['items'])} reviews, got {len(reviews)}", file=sys.stderr)
        sys.exit(1)

    # Group evaluation by rate
    rates_stats = {}
    for r in sealed["rates"]:
        rates_stats[r] = {
            "total": 0,
            "correct_amount": 0,
            "readability_scores": [],
            "naturalness_scores": [],
            "clipping_count": 0,
            "abrupt_count": 0,
            "missing_count": 0,
            "errors": [],
        }

    for rev in reviews:
        tid = rev["testId"]
        meta = mapping_by_id.get(tid)
        if not meta:
            raise ValueError(f"Unknown sample ID: {tid}")
        rate = meta["rate"]
        st = rates_stats[rate]
        st["total"] += 1

        heard = str(rev.get("heardAmount", "")).strip().replace(".", "").replace(",", "")
        expected = str(meta.get("amountVnd", "")).strip()

        if heard == "" or rev.get("readability") is None or rev.get("naturalness") is None:
            print(f"Validation incomplete: {tid} has empty review fields", file=sys.stderr)
            sys.exit(1)

        for field in ("readability", "naturalness"):
            if type(rev.get(field)) is not int or not 1 <= rev[field] <= 5:
                raise ValueError(f"{tid}: {field} must be an integer from 1 to 5")
        for field in ("clipping", "abruptJoin", "missingWords"):
            if type(rev.get(field)) is not bool:
                raise ValueError(f"{tid}: {field} requires an explicit human assessment")
        readability = rev["readability"]
        naturalness = rev["naturalness"]
        st["readability_scores"].append(readability)
        st["naturalness_scores"].append(naturalness)

        if rev.get("clipping"):
            st["clipping_count"] += 1
        if rev.get("abruptJoin"):
            st["abrupt_count"] += 1
        if rev.get("missingWords"):
            st["missing_count"] += 1

        if heard == expected:
            st["correct_amount"] += 1
        else:
            st["errors"].append(f"{tid}: heard {heard} != expected {expected}")

    # Determine passing rates
    passing_rates = []
    print("\n--- Blind Listening Evaluation Results ---")
    for rate, st in rates_stats.items():
        min_readability = min(st["readability_scores"]) if st["readability_scores"] else 0
        median_readability = statistics.median(st["readability_scores"]) if st["readability_scores"] else 0
        mean_readability = statistics.mean(st["readability_scores"]) if st["readability_scores"] else 0
        median_naturalness = statistics.median(st["naturalness_scores"]) if st["naturalness_scores"] else 0
        amount_accuracy = st["correct_amount"] / st["total"] if st["total"] else 0

        passed = (
            st["total"] == 50
            and st["correct_amount"] == 50
            and st["clipping_count"] == 0
            and st["missing_count"] == 0
            and st["abrupt_count"] == 0
            and min_readability >= 4
            and median_naturalness >= 4
        )

        st["passed"] = passed
        st["median_naturalness"] = median_naturalness
        st["mean_readability"] = mean_readability

        print(f"Rate {rate:4.2f}: Accuracy={st['correct_amount']}/{st['total']} ({amount_accuracy*100:.1f}%), "
              f"Readability(min={min_readability}, mean={mean_readability:.2f}), "
              f"Naturalness(median={median_naturalness:.1f}), "
              f"Clipping={st['clipping_count']} -> {'PASS' if passed else 'FAIL'}")

        if passed:
            passing_rates.append(rate)

    if not passing_rates and review_data.get("overrideRate") is None:
        print("No rates passed the strict quality acceptance criteria!", file=sys.stderr)
        sys.exit(2)

    # Select best rate according to tie-breaking rules
    override = review_data.get("overrideRate")
    if override is not None:
        # An override may select among passing rates, never bypass quality gates.
        override_st = rates_stats.get(float(override))
        if not override_st or not override_st["passed"]:
            print(f"Override rate {override} failed quality acceptance gates!", file=sys.stderr)
            sys.exit(3)
        selected_rate = float(override)
        print(f"Using approved user override rate: {selected_rate}")
    else:
        # Sort by:
        # 1. median naturalness descending
        # 2. mean readability descending
        # 3. distance to 1.10 ascending
        # 4. rate value ascending
        passing_rates.sort(key=lambda r: (
            -rates_stats[r]["median_naturalness"],
            -rates_stats[r]["mean_readability"],
            abs(r - 1.10),
            r,
        ))
        selected_rate = passing_rates[0]
        print(f"Automatically selected best rate: {selected_rate}")

    # Output profile
    profile = {
        "providerMode": "PIPER_ONLY",
        "voiceId": VOICE_ID,
        "rate": selected_rate,
        "pitchHz": 0,
        "catalogVersion": "v1",
    }
    profile_out.parent.mkdir(parents=True, exist_ok=True)
    with profile_out.open("w", encoding="utf-8") as f:
        json.dump(profile, f, indent=2)

    print(f"Saved approved voice profile to: {profile_out}")


def run_render_comparisons(args):
    pack_root = Path(args.pack).resolve()
    corpus_path = Path(args.corpus)
    out_dir = Path(args.output)
    candidates = list(pack_root.glob("*/manifest.candidate.json"))
    if (pack_root / "manifest.candidate.json").exists():
        candidates.append(pack_root / "manifest.candidate.json")
    if len(candidates) != 1:
        raise ValueError("Require exactly one candidate manifest; choose an unambiguous pack directory")
    manifest = json.loads(candidates[0].read_text(encoding="utf-8"))
    inputs = manifest_inputs(manifest)
    validate_inputs(inputs)
    corpus = json.loads(corpus_path.read_text(encoding="utf-8"))
    audio_root = pack_root if (pack_root / "clips").is_dir() else pack_root.parent
    cards = []
    for clip in manifest["clips"]:
        read_clip(audio_root, clip)
        audio_uri = (audio_root / "clips" / (clip["sha256"] + ".mp3")).as_uri()
        cards.append(f'<p>{html.escape(clip["clipId"])}: {html.escape(clip["transcript"])}</p>'
                     f'<audio controls preload="none" src="{html.escape(audio_uri, quote=True)}"></audio>')
    evidence = {
        "schemaVersion": 1,
        "synthesisKey": manifest["synthesisKey"],
        "inputHash": hashlib.sha256(canonical_json(inputs)).hexdigest(),
        "reviewer": "", "reviewedAt": "", "decision": "pending",
        "clips": [{"clipId": c["clipId"], "sha256": c["sha256"], "speechStartMs": None, "amountStartMs": None} for c in manifest["clips"]],
        "heardClipIds": [], "heardRecipePairIds": [],
        "amountAccuracy": {"correct": None, "total": None},
        "readabilityMedian": None, "naturalnessMedian": None,
        "joinFailures": [], "rejectedClipIds": [],
        "candidateDecisions": {"tail": "pending", "numbers": "pending"},
        "tailMode": None, "numberMode": None, "selectedRate": manifest["rate"],
        "sampleSetSha256": hashlib.sha256(canonical_json(corpus)).hexdigest(),
    }
    out_dir.mkdir(parents=True, exist_ok=True)
    if (out_dir / "quality.json").exists() or (out_dir / "candidate-player.html").exists():
        raise ValueError("Refusing to overwrite existing review artifacts; choose a fresh output directory")
    (out_dir / "quality.json").write_text(json.dumps(evidence, indent=2, ensure_ascii=False), encoding="utf-8")
    (out_dir / "candidate-player.html").write_text(
        '<!doctype html><html lang="vi"><meta charset="utf-8"><title>Unreviewed candidate clips</title>'
        '<h1>Candidate clips — not approved</h1><p>Clip inventory only; whole-vs-composed comparisons have NOT been rendered. '
        'No listening, onset or quality assessment is inferred from playback controls.</p>' + ''.join(cards) + '</html>',
        encoding="utf-8",
    )
    print(f"Wrote hash-checked clip player and pending review template in {out_dir}")
    raise RuntimeError(
        "BLOCKED: real whole-vs-fragment/number comparison rendering is not implemented. "
        "Template and clip player are not comparison evidence; require real comparison audio and human review."
    )


def main():
    parser = argparse.ArgumentParser(description="Voice Quality Runner")
    subparsers = parser.add_subparsers(dest="subcommand", required=True)

    prep = subparsers.add_parser("prepare")
    prep.add_argument("--corpus", required=True, help="Path to quality-corpus.json")
    prep.add_argument("--provider", default="piper")
    prep.add_argument("--rates", default="1.0,1.1,1.15,1.25")
    prep.add_argument("--output", required=True, help="Directory for output")

    val = subparsers.add_parser("validate")
    val.add_argument("--review", required=True, help="Path to completed review.json")
    val.add_argument("--profile-output", required=True, help="Path to output profile.json")

    comp = subparsers.add_parser("render-comparisons")
    comp.add_argument("--pack", required=True, help="Directory of candidate voice pack")
    comp.add_argument("--corpus", required=True, help="Path to quality-corpus.json")
    comp.add_argument("--output", required=True, help="Output directory")

    args = parser.parse_args()
    if args.subcommand == "prepare":
        asyncio.run(run_prepare(args))
    elif args.subcommand == "validate":
        run_validate(args)
    elif args.subcommand == "render-comparisons":
        run_render_comparisons(args)


if __name__ == "__main__":
    main()
