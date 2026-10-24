#!/usr/bin/env python3
"""Prepare and validate human listening evidence for fixed-profile VieNeu audio."""
import argparse
import asyncio
import hashlib
import json
from datetime import datetime
import html
from pathlib import Path
import random
import statistics
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from app.assets import canonical_json, manifest_inputs, read_clip, validate_inputs
from app.providers.vieneu import VieneuTTSProvider, VOICE_ID as VIENEU_VOICE_ID

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

    if args.provider != "vieneu":
        raise ValueError("Provider must be 'vieneu'")
    rates = [float(r.strip()) for r in args.rates.split(",") if r.strip()]
    if rates != [1.0]:
        raise ValueError("VieNeu only supports rate 1.0")
    all_cases = corpus["cases"]
    amount_cases = [c for c in all_cases if c["kind"] == "amount"]
    if len(amount_cases) != 50 or {str(c.get("amountVnd")) for c in amount_cases} != set(CANONICAL_AMOUNTS_50):
        raise ValueError("Corpus must contain exactly the 50 distinct canonical amount cases")
    if len({c["id"] for c in all_cases}) != len(all_cases):
        raise ValueError("Corpus case IDs must be unique")
    if (out_dir / "review.json").exists() and not (out_dir / "audio").exists():
        raise ValueError("Refusing to overwrite existing listening evidence; choose a fresh output directory")

    cases_to_use = all_cases if getattr(args, "all_cases", False) else amount_cases
    provider = VieneuTTSProvider(threads=2)
    active_voice = VIENEU_VOICE_ID
    await provider.start()

    tasks_to_generate = []
    for r in rates:
        for c in cases_to_use:
            tasks_to_generate.append({
                "caseId": c["id"],
                "kind": c.get("kind", "amount"),
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
            rate_str = "+0%"
            audio_file = audio_dir / f"{test_id}.mp3"
            if audio_file.exists() and audio_file.stat().st_size > 0:
                audio_bytes = audio_file.read_bytes()
            else:
                audio_bytes = await provider.synthesize(item["transcript"], voice=active_voice, rate=rate_str)
                audio_file.write_bytes(audio_bytes)
            audio_sha = hashlib.sha256(audio_bytes).hexdigest()

            mapping.append({
                "testId": test_id,
                "caseId": item["caseId"],
                "kind": item["kind"],
                "amountVnd": item["amountVnd"],
                "rate": item["rate"],
                "transcript": item["transcript"],
                "audioSha256": audio_sha,
                "audioFile": f"audio/{test_id}.mp3",
            })

            blank_reviews.append({
                "testId": test_id,
                "heardAmount": "",
                "heardText": "",
                "readability": None,
                "naturalness": None,
                "abruptJoin": None,
                "clipping": None,
                "missingWords": None,
                "repeatedWords": None,
                "notes": "",
            })
    finally:
        await provider.close()

    # Save sealed mapping
    prov_manifest_path = Path(__file__).resolve().parents[1] / "assets" / "models" / "vieneu" / "provenance.json"
    runtime_sha = hashlib.sha256(prov_manifest_path.read_bytes()).hexdigest() if prov_manifest_path.exists() else ""
    mapping_path = out_dir / "mapping.sealed.json"
    with mapping_path.open("w", encoding="utf-8") as f:
        json.dump({
            "schemaVersion": 2,
            "providerMode": "VIENEU_ONLY",
            "voiceId": active_voice,
            "runtimeManifestSha256": runtime_sha,
            "rates": rates,
            "items": mapping,
        }, f, indent=2, ensure_ascii=False)

    review_path = out_dir / "review.json"
    with review_path.open("w", encoding="utf-8") as f:
        json.dump({
            "schemaVersion": 2,
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
    if (sealed.get("schemaVersion") != 2 or sealed.get("providerMode") != "VIENEU_ONLY"
            or sealed.get("voiceId") != VIENEU_VOICE_ID or sealed.get("rates") != [1.0]
            or any(type(item.get("rate")) not in (int, float) or item["rate"] != 1.0 for item in sealed["items"])):
        raise ValueError("Listening evidence must use the fixed VieNeu profile at rate 1.0")

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
        amount_items = [m for m in items if m.get("kind", "amount") == "amount"]
        if len(amount_items) != 50 or {str(m.get("amountVnd")) for m in amount_items} != set(CANONICAL_AMOUNTS_50):
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
        kind = meta.get("kind", "amount")
        rate = meta["rate"]
        st = rates_stats[rate]
        st["total"] += 1

        for field in ("readability", "naturalness"):
            if type(rev.get(field)) is not int or not 1 <= rev[field] <= 5:
                raise ValueError(f"{tid}: {field} must be an integer from 1 to 5")
        for field in ("clipping", "abruptJoin", "missingWords"):
            if type(rev.get(field)) is not bool:
                raise ValueError(f"{tid}: {field} requires an explicit human assessment")
        if "repeatedWords" in rev and type(rev.get("repeatedWords")) is not bool:
            raise ValueError(f"{tid}: repeatedWords requires an explicit boolean assessment")

        readability = rev["readability"]
        naturalness = rev["naturalness"]
        st["readability_scores"].append(readability)
        st["naturalness_scores"].append(naturalness)

        if rev.get("clipping"):
            st["clipping_count"] += 1
        if rev.get("abruptJoin"):
            st["abrupt_count"] += 1
        if rev.get("missingWords") or rev.get("repeatedWords"):
            st["missing_count"] += 1

        if kind == "amount" or meta.get("amountVnd"):
            expected = str(meta.get("amountVnd", "")).strip()
            heard = str(rev.get("heardAmount", "")).strip().replace(".", "").replace(",", "")
            if heard == expected:
                st["correct_amount"] += 1
            else:
                st["errors"].append(f"{tid}: heard {heard} != expected {expected}")
        else:
            heard_text = str(rev.get("heardText", "")).strip()
            if not heard_text:
                st["errors"].append(f"{tid}: empty heardText for non-amount case")
    # Determine passing rates
    passing_rates = []
    print("\n--- Blind Listening Evaluation Results ---")
    for rate, st in rates_stats.items():
        min_readability = min(st["readability_scores"]) if st["readability_scores"] else 0
        median_readability = statistics.median(st["readability_scores"]) if st["readability_scores"] else 0
        mean_readability = statistics.mean(st["readability_scores"]) if st["readability_scores"] else 0
        median_naturalness = statistics.median(st["naturalness_scores"]) if st["naturalness_scores"] else 0
        amount_accuracy = st["correct_amount"] / st["total"] if st["total"] else 0
        amount_items_count = sum(1 for m in sealed["items"] if m["rate"] == rate and (m.get("kind") == "amount" or m.get("amountVnd")))
        passed = (
            st["correct_amount"] == amount_items_count
            and len(st["errors"]) == 0
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

    override = review_data.get("overrideRate")
    if override is not None and (type(override) not in (int, float) or override != 1.0):
        raise ValueError("VieNeu only supports rate 1.0")
    if 1.0 not in passing_rates:
        print("VieNeu rate 1.0 failed quality acceptance gates!", file=sys.stderr)
        sys.exit(3)
    selected_rate = 1.0
    # Output profile
    profile = {
        "providerMode": "VIENEU_ONLY",
        "voiceId": VIENEU_VOICE_ID,
        "rate": selected_rate,
        "pitchHz": 0,
        "catalogVersion": "v1",
    }
    profile_out.parent.mkdir(parents=True, exist_ok=True)
    with profile_out.open("w", encoding="utf-8") as f:
        json.dump(profile, f, indent=2)

    print(f"Saved approved voice profile to: {profile_out}")


def run_prepare_pack_review(args):
    pack_dir = Path(args.pack).resolve()
    corpus_path = Path(args.corpus)
    out_dir = Path(args.output)

    candidate_files = list(pack_dir.glob("*/manifest.candidate.json"))
    if (pack_dir / "manifest.candidate.json").exists():
        candidate_files.append(pack_dir / "manifest.candidate.json")
    if len(candidate_files) != 1:
        raise ValueError("Require exactly one candidate manifest in pack directory")
    manifest = json.loads(candidate_files[0].read_text(encoding="utf-8"))
    if manifest.get("voiceId") != VIENEU_VOICE_ID:
        raise ValueError(f"prepare-pack-review only supports candidate VieNeu packs, got {manifest.get('voiceId')}")
    comp = manifest.get("composition", {})
    if comp.get("tail") != "whole" or comp.get("numbers") != "whole":
        raise ValueError("prepare-pack-review requires whole-only candidate composition")

    inputs = manifest_inputs(manifest)
    validate_inputs(inputs)
    corpus = json.loads(corpus_path.read_text(encoding="utf-8"))

    audio_root = pack_dir if (pack_dir / "clips").is_dir() else pack_dir.parent
    out_dir.mkdir(parents=True, exist_ok=True)
    if (out_dir / "review.json").exists():
        raise ValueError("Refusing to overwrite existing pack review.json")

    cards = []
    review_items = []
    for idx, clip in enumerate(manifest["clips"]):
        read_clip(audio_root, clip)
        test_id = f"pack-{idx+1:03d}"
        audio_path = (audio_root / "clips" / (clip["sha256"] + ".mp3")).resolve()
        cards.append(
            f'<div class="card">'
            f'<strong>{test_id} ({clip["kind"]})</strong>'
            f'<p>{html.escape(clip["transcript"])}</p>'
            f'<audio controls src="{audio_path.as_uri()}"></audio>'
            f'</div>'
        )
        review_items.append({
            "testId": test_id,
            "clipId": clip["clipId"],
            "sha256": clip["sha256"],
            "kind": clip["kind"],
            "readability": None,
            "naturalness": None,
            "clipping": None,
            "missingWords": None,
            "repeatedWords": None,
            "approved": None,
            "notes": "",
        })

    # Create player
    player_html = (
        '<!doctype html><html lang="vi"><meta charset="utf-8"><title>VieNeu Pack Review Player</title>'
        '<style>body{font-family:sans-serif;max-width:800px;margin:2rem auto;padding:0 1rem;}'
        '.card{border:1px solid #ccc;padding:1rem;margin-bottom:1rem;border-radius:8px;}'
        'audio{width:100%;margin-top:0.5rem;}</style>'
        f'<h1>VieNeu Published Pack Review ({len(manifest["clips"])} clips)</h1>'
        + "".join(cards)
        + "</html>"
    )
    (out_dir / "player.html").write_text(player_html, encoding="utf-8")

    review_template = {
        "schemaVersion": 2,
        "synthesisKey": manifest["synthesisKey"],
        "packVersion": manifest["packVersion"],
        "inputHash": hashlib.sha256(canonical_json(inputs)).hexdigest(),
        "sampleSetSha256": hashlib.sha256(canonical_json(corpus)).hexdigest(),
        "reviewer": "",
        "reviewedAt": "",
        "amountAccuracy": {"correct": None, "total": None},
        "reviews": review_items,
    }
    (out_dir / "review.json").write_text(json.dumps(review_template, indent=2, ensure_ascii=False), encoding="utf-8")
    print(f"Prepared pack review player and review template for {len(manifest['clips'])} clips in {out_dir}")


def run_validate_pack_review(args):
    review_file = Path(args.review)
    quality_out = Path(args.quality_output)
    review_data = json.loads(review_file.read_text(encoding="utf-8"))

    if not isinstance(review_data.get("reviewer"), str) or not review_data["reviewer"].strip():
        raise ValueError("A named human reviewer is required")
    reviewed_at = datetime.fromisoformat(review_data.get("reviewedAt", "").replace("Z", "+00:00"))
    if reviewed_at.tzinfo is None:
        raise ValueError("reviewedAt must include timezone")

    reviews = review_data.get("reviews", [])
    if not reviews:
        raise ValueError("Empty reviews in pack review")

    readability_scores = []
    naturalness_scores = []
    heard_clip_ids = []
    clips_evidence = []

    accuracy = review_data.get("amountAccuracy")
    if (not isinstance(accuracy, dict) or set(accuracy) != {"correct", "total"}
            or type(accuracy["correct"]) is not int or type(accuracy["total"]) is not int
            or accuracy["total"] < 50 or accuracy["correct"] != accuracy["total"]):
        raise ValueError("Explicit human amount review requires zero errors across at least 50 cases")

    for r in reviews:
        cid = r["clipId"]
        heard_clip_ids.append(cid)
        if r.get("approved") is not True:
            raise ValueError(f"Clip {cid} was not explicitly approved by reviewer")
        for field in ("clipping", "missingWords", "repeatedWords"):
            if r.get(field) is True:
                raise ValueError(f"Clip {cid} has unacceptable defect: {field}=True")
            if type(r.get(field)) is not bool:
                raise ValueError(f"Clip {cid} requires human boolean check for {field}")
        for field in ("readability", "naturalness"):
            score = r.get(field)
            if type(score) is not int or not 1 <= score <= 5:
                raise ValueError(f"Clip {cid}: {field} must be integer 1..5")
            if field == "readability" and score < 4:
                raise ValueError(f"Clip {cid} readability score {score} < 4")
        readability_scores.append(r["readability"])
        naturalness_scores.append(r["naturalness"])
        clips_evidence.append({
            "clipId": cid,
            "sha256": r["sha256"],
            "speechStartMs": None,
            "amountStartMs": None,
        })

    median_nat = statistics.median(naturalness_scores) if naturalness_scores else 0
    if median_nat < 4:
        raise ValueError(f"Median naturalness {median_nat} < 4")
    median_read = statistics.median(readability_scores) if readability_scores else 0

    evidence = {
        "schemaVersion": 1,
        "synthesisKey": review_data["synthesisKey"],
        "inputHash": review_data["inputHash"],
        "reviewer": review_data["reviewer"],
        "reviewedAt": review_data["reviewedAt"],
        "decision": "approved",
        "clips": clips_evidence,
        "heardClipIds": heard_clip_ids,
        "heardRecipePairIds": [],
        "amountAccuracy": accuracy,
        "readabilityMedian": float(median_read),
        "naturalnessMedian": float(median_nat),
        "joinFailures": [],
        "rejectedClipIds": [],
        "candidateDecisions": {"tail": "rejected", "numbers": "rejected"},
        "tailMode": "whole",
        "numberMode": "whole",
        "selectedRate": 1.0,
        "sampleSetSha256": review_data["sampleSetSha256"],
    }

    quality_out.parent.mkdir(parents=True, exist_ok=True)
    quality_out.write_text(json.dumps(evidence, indent=2, ensure_ascii=False), encoding="utf-8")
    print(f"Generated approved quality evidence: {quality_out}")


def run_benchmark(args):
    import concurrent.futures
    import time
    import urllib.request

    base_url = args.base_url.rstrip("/")
    token_path = Path(args.token_file)
    token = token_path.read_text(encoding="utf-8").strip() if token_path.exists() else ""
    corpus = json.loads(Path(args.corpus).read_text(encoding="utf-8"))
    out_dir = Path(args.output)
    out_dir.mkdir(parents=True, exist_ok=True)

    cases = corpus["cases"]
    print(f"Running benchmark on {len(cases)} cases, repeats={args.repeats}, concurrency={args.concurrency}...")

    def run_single_request(case):
        url = f"{base_url}/synthesize/stream"
        payload = json.dumps({
            "text": case["transcript"],
            "voice": VIENEU_VOICE_ID,
            "provider_mode": "VIENEU_ONLY",
            "rate": "+0%",
            "pitch": "+0Hz",
            "cacheable": False,
            "timeout_ms": 8000,
        }).encode("utf-8")
        req = urllib.request.Request(
            url,
            data=payload,
            headers={
                "Content-Type": "application/json",
                "X-Internal-TTS-Token": token,
            },
        )
        start_t = time.perf_counter()
        first_byte_t = None
        total_bytes = 0
        status = 0
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                status = resp.status
                while chunk := resp.read(4096):
                    if first_byte_t is None:
                        first_byte_t = time.perf_counter()
                    total_bytes += len(chunk)
            end_t = time.perf_counter()
            ttfb = (first_byte_t - start_t) if first_byte_t else (end_t - start_t)
            completion = end_t - start_t
            return {
                "caseId": case["id"],
                "kind": case.get("kind"),
                "status": status,
                "bytes": total_bytes,
                "ttfbSeconds": ttfb,
                "completionSeconds": completion,
                "error": None,
            }
        except Exception as exc:
            end_t = time.perf_counter()
            return {
                "caseId": case["id"],
                "kind": case.get("kind"),
                "status": status,
                "bytes": total_bytes,
                "ttfbSeconds": None,
                "completionSeconds": end_t - start_t,
                "error": str(exc),
            }

    all_results = []
    for rep in range(args.repeats):
        print(f"Repetition {rep+1}/{args.repeats}...")
        if args.concurrency <= 1:
            for c in cases:
                r = run_single_request(c)
                r["repetition"] = rep + 1
                all_results.append(r)
        else:
            with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as ex:
                futures = [ex.submit(run_single_request, c) for c in cases]
                for f in concurrent.futures.as_completed(futures):
                    res = f.result()
                    res["repetition"] = rep + 1
                    all_results.append(res)

    out_file = out_dir / f"benchmark-c{args.concurrency}.json"
    out_file.write_text(json.dumps(all_results, indent=2, ensure_ascii=False), encoding="utf-8")
    print(f"Saved benchmark report to {out_file}")




def main():
    parser = argparse.ArgumentParser(description="Voice Quality Runner")
    subparsers = parser.add_subparsers(dest="subcommand", required=True)

    prep = subparsers.add_parser("prepare")
    prep.add_argument("--corpus", required=True, help="Path to quality-corpus.json")
    prep.add_argument("--provider", choices=("vieneu",), default="vieneu")
    prep.add_argument("--rates", default="1.0")
    prep.add_argument("--all-cases", action="store_true", help="Render all cases in corpus, not only amount")
    prep.add_argument("--output", required=True, help="Directory for output")

    val = subparsers.add_parser("validate")
    val.add_argument("--review", required=True, help="Path to completed review.json")
    val.add_argument("--profile-output", required=True, help="Path to output profile.json")


    pack_prep = subparsers.add_parser("prepare-pack-review")
    pack_prep.add_argument("--pack", required=True, help="Directory of candidate whole-only voice pack")
    pack_prep.add_argument("--corpus", required=True, help="Path to quality-corpus.json")
    pack_prep.add_argument("--output", required=True, help="Directory for pack review artifacts")

    pack_val = subparsers.add_parser("validate-pack-review")
    pack_val.add_argument("--review", required=True, help="Path to completed pack review.json")
    pack_val.add_argument("--quality-output", required=True, help="Path to output quality.json")

    bench = subparsers.add_parser("benchmark")
    bench.add_argument("--base-url", required=True, help="Base URL of TTS service")
    bench.add_argument("--token-file", required=True, help="Path to internal token file")
    bench.add_argument("--corpus", required=True, help="Path to quality-corpus.json")
    bench.add_argument("--output", required=True, help="Directory to save benchmark results")
    bench.add_argument("--repeats", type=int, default=3, help="Number of repetitions per case")
    bench.add_argument("--concurrency", type=int, default=1, help="Concurrent stream requests")

    args = parser.parse_args()
    if args.subcommand == "prepare":
        asyncio.run(run_prepare(args))
    elif args.subcommand == "validate":
        run_validate(args)
    elif args.subcommand == "prepare-pack-review":
        run_prepare_pack_review(args)
    elif args.subcommand == "validate-pack-review":
        run_validate_pack_review(args)
    elif args.subcommand == "benchmark":
        run_benchmark(args)

if __name__ == "__main__":
    main()
