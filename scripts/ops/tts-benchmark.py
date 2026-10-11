#!/usr/bin/env python3
"""Local Piper provider timing; not a container, HTTP, or release benchmark."""
import argparse
import asyncio
import json
import math
from pathlib import Path
import platform
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "tts-gateway"))
from app.providers.piper import PiperTTSProvider, VOICE_ID


def format_rate(rate: float) -> str:
    return f"{int(round((rate - 1.0) * 100)):+d}%"


async def benchmark_worker_arm(worker_count: int, corpus_cases: list, rate: float, repetitions: int, requests_per_run: int):
    provider = PiperTTSProvider(worker_count=worker_count)
    await provider.start()

    results = []
    print(f"\n--- Benchmarking Worker Count: {worker_count} (Rate: {rate}, Reps: {repetitions}, Requests/Run: {requests_per_run}) ---")

    try:
        for rep in range(repetitions):
            print(f"Repetition {rep + 1}/{repetitions}...")
            # 1. Warmup (10 requests outside sample)
            for i in range(10):
                c = corpus_cases[i % len(corpus_cases)]
                await provider.synthesize(c["transcript"], voice=VOICE_ID, rate=format_rate(rate))

            # 2. Stream serial 1
            print(f"  Measuring serial streaming ({requests_per_run} requests)...")
            for req_idx in range(requests_per_run):
                c = corpus_cases[req_idx % len(corpus_cases)]
                t0 = time.monotonic()
                first_byte_t = None
                total_bytes = 0

                generator = provider.stream(c["transcript"], voice=VOICE_ID, rate=format_rate(rate))
                try:
                    async for chunk in generator:
                        if first_byte_t is None and chunk:
                            first_byte_t = time.monotonic()
                        total_bytes += len(chunk)
                except Exception as err:
                    results.append({
                        "scenario": "stream-serial-1",
                        "workers": worker_count,
                        "repeat": rep + 1,
                        "caseId": c["id"],
                        "status": None,
                        "outcome": "failed",
                        "firstByteMs": (first_byte_t - t0) * 1000 if first_byte_t is not None else None,
                        "totalMs": (time.monotonic() - t0) * 1000,
                        "bytes": total_bytes,
                        "error": str(err),
                    })
                    continue

                t_end = time.monotonic()
                fb_ms = (first_byte_t - t0) * 1000 if first_byte_t else None
                total_ms = (t_end - t0) * 1000

                results.append({
                    "scenario": "stream-serial-1",
                    "workers": worker_count,
                    "repeat": rep + 1,
                    "caseId": c["id"],
                    "status": None,
                    "outcome": "completed" if total_bytes else "failed",
                    "firstByteMs": fb_ms,
                    "totalMs": total_ms,
                    "bytes": total_bytes,
                })

            # 3. Stream concurrency 2
            print(f"  Measuring concurrent streaming (concurrency 2, {requests_per_run} requests)...")
            sem = asyncio.Semaphore(2)

            async def run_concurrent(c_item):
                async with sem:
                    t_start = time.monotonic()
                    fb_t = None
                    t_bytes = 0
                    gen = provider.stream(c_item["transcript"], voice=VOICE_ID, rate=format_rate(rate))
                    try:
                        async for chunk in gen:
                            if fb_t is None and chunk:
                                fb_t = time.monotonic()
                            t_bytes += len(chunk)
                        fb_val = (fb_t - t_start) * 1000 if fb_t else None
                        return {
                            "scenario": "stream-concurrency-2",
                            "workers": worker_count,
                            "repeat": rep + 1,
                            "caseId": c_item["id"],
                            "status": None,
                            "outcome": "completed" if t_bytes else "failed",
                            "firstByteMs": fb_val,
                            "totalMs": (time.monotonic() - t_start) * 1000,
                            "bytes": t_bytes,
                        }
                    except Exception as exc:
                        return {
                            "scenario": "stream-concurrency-2",
                            "workers": worker_count,
                            "repeat": rep + 1,
                            "caseId": c_item["id"],
                            "status": None,
                            "outcome": "failed",
                            "firstByteMs": (fb_t - t_start) * 1000 if fb_t is not None else None,
                            "totalMs": (time.monotonic() - t_start) * 1000,
                            "bytes": t_bytes,
                            "error": str(exc),
                        }

            tasks = [run_concurrent(corpus_cases[i % len(corpus_cases)]) for i in range(requests_per_run)]
            concurrent_res = await asyncio.gather(*tasks)
            results.extend(concurrent_res)

    finally:
        await provider.close()

    return results


def calculate_metrics(attempts: list):
    completed = [a for a in attempts if a.get("outcome") == "completed"]

    first_bytes = [a["firstByteMs"] for a in completed if a.get("firstByteMs") is not None]
    totals = [a["totalMs"] for a in completed if a.get("totalMs") is not None]

    def percentile(values, p):
        if not values:
            return None
        sorted_vals = sorted(values)
        k = (len(sorted_vals) - 1) * p
        f = math.floor(k)
        c = math.ceil(k)
        if f == c:
            return sorted_vals[int(k)]
        return sorted_vals[f] * (c - k) + sorted_vals[c] * (k - f)

    return {
        "n": len(completed),
        "totalAttempts": len(attempts),
        "failedAttempts": len(attempts) - len(completed),
        "firstByteP50Ms": percentile(first_bytes, 0.50),
        "firstByteP95Ms": percentile(first_bytes, 0.95),
        "firstByteP99Ms": percentile(first_bytes, 0.99),
        "totalDurationP50Ms": percentile(totals, 0.50),
        "totalDurationP95Ms": percentile(totals, 0.95),
        "totalDurationP99Ms": percentile(totals, 0.99),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="", help="Unsupported: this runner does not launch an image")
    parser.add_argument("--workers", default="1,2", help="Comma-separated worker counts to test")
    parser.add_argument("--corpus", required=True, help="Path to quality corpus JSON")
    parser.add_argument("--rate", type=float, required=True, help="Explicit rate to measure; not evidence of approval")
    parser.add_argument("--repetitions", type=int, default=3, help="Repetitions per arm")
    parser.add_argument("--requests", type=int, default=100, help="Requests per repetition (default 100 for dev)")
    parser.add_argument("--soak", default="", help="Unsupported: this runner does not implement a soak")
    parser.add_argument("--output", required=True, help="Output directory")

    args = parser.parse_args()
    if args.image or args.soak:
        parser.error("Image/quota and soak benchmarking are not implemented; no production evidence will be generated")
    try:
        worker_counts = [int(w.strip()) for w in args.workers.split(",")]
    except ValueError:
        parser.error("Workers must be 1 and/or 2")
    if not worker_counts or len(set(worker_counts)) != len(worker_counts) or any(w not in (1, 2) for w in worker_counts):
        parser.error("Workers must be unique values 1 and/or 2")
    if args.requests < 1 or args.repetitions < 1 or not math.isfinite(args.rate) or not 0.8 <= args.rate <= 1.5:
        parser.error("Require positive request/repetition counts and a finite supported rate (0.8–1.5)")
    out_dir = Path(args.output)
    out_dir.mkdir(parents=True, exist_ok=True)

    with open(args.corpus, "r", encoding="utf-8") as f:
        corpus = json.load(f)

    if not corpus.get("cases"):
        parser.error("Corpus must contain cases")

    report = {
        "schemaVersion": 1,
        "scope": "local-provider-only",
        "releaseGate": "BLOCKED",
        "limitations": ["No image execution or digest verification", "No enforced cgroup quotas or resource sampling", "No HTTP/admission/cache/burst/cancellation scenarios", "No soak", "No ARM64 production or browser/human quality evidence"],
        "benchmarkAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "environment": {
            "os": sys.platform,
            "architecture": platform.machine(),
            "pythonVersion": platform.python_version(),
            "processor": platform.processor(),
        },
        "parameters": {
            "rate": args.rate,
            "repetitions": args.repetitions,
            "requestsPerRun": args.requests,
            "workerCounts": worker_counts,
        },
        "arms": {},
    }

    all_attempts = {}

    for w in worker_counts:
        attempts = asyncio.run(benchmark_worker_arm(
            worker_count=w,
            corpus_cases=corpus["cases"],
            rate=args.rate,
            repetitions=args.repetitions,
            requests_per_run=args.requests,
        ))
        all_attempts[w] = attempts

        by_scenario = {}
        for a in attempts:
            by_scenario.setdefault(a["scenario"], []).append(a)

        report["arms"][str(w)] = {
            "metrics": {sc: calculate_metrics(items) for sc, items in by_scenario.items()},
        }

    report["decision"] = {
        "selectedWorkers": None,
        "status": "BLOCKED",
        "reason": "Local provider timings cannot select production workers; quota, throughput, resource and soak gates are unmeasured",
    }
    print("Local timing only; production worker selection is BLOCKED")

    out_file = out_dir / "benchmark-report.json"
    with open(out_file, "w", encoding="utf-8") as f:
        json.dump(report, f, indent=2)

    raw_attempts_file = out_dir / "attempts.json"
    with open(raw_attempts_file, "w", encoding="utf-8") as f:
        json.dump(all_attempts, f, indent=2)

    print(f"Report written to: {out_file}")


if __name__ == "__main__":
    main()
