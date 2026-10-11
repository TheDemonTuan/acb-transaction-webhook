"""Two persistent CPU subprocesses with kill-and-reap cancellation ownership."""
import asyncio
import hashlib
import json
import os
from pathlib import Path
import struct
import sys

import anyio

VOICE_ID = "vi-VN-vais1000-medium"
MODEL_SHA256 = "ec7c89e2c85f4d1edc24b6120c18aaf1bda614f06b511567eb9c7c0de15e2dab"
CONFIG_SHA256 = "fafb9da1354ed4b77c31af228ed41fb41cd825c14cffa105454b25e6ae751ee0"
MAX_AUDIO_BYTES = 4 * 1024 * 1024
DEFAULT_MODEL = Path(__file__).resolve().parents[2] / "assets" / "models" / (VOICE_ID + ".onnx")


class PiperTTSProvider:
    def __init__(self, model_path=DEFAULT_MODEL, worker_count: int = 2):
        if worker_count not in (1, 2):
            raise ValueError(f"worker_count must be 1 or 2, got {worker_count}")
        self.model_path = Path(model_path)
        self.worker_count = worker_count
        self._workers = [None] * worker_count
        self._idle = asyncio.Queue(maxsize=worker_count)
        self._closed = True

    @property
    def idle_count(self) -> int:
        return self._idle.qsize()

    @property
    def active_count(self) -> int:
        return self.worker_count - self._idle.qsize()
    @property
    def available(self):
        return not self._closed and all(worker is not None and worker.returncode is None for worker in self._workers)

    def _verify_model(self):
        for path, digest in ((self.model_path, MODEL_SHA256), (Path(str(self.model_path) + ".json"), CONFIG_SHA256)):
            hasher = hashlib.sha256()
            with path.open("rb") as source:
                for chunk in iter(lambda: source.read(1024 * 1024), b""):
                    hasher.update(chunk)
            if hasher.hexdigest() != digest:
                raise RuntimeError("Fixed Piper model checksum mismatch")

    async def _spawn(self, index):
        if self._closed:
            raise RuntimeError("Piper is closed")
        environment = dict(os.environ, OMP_NUM_THREADS="1", OPENBLAS_NUM_THREADS="1", MKL_NUM_THREADS="1")
        launch = asyncio.create_task(asyncio.create_subprocess_exec(
            sys.executable, "-m", "app.providers.piper_worker", str(self.model_path),
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=None, env=environment,
            cwd=str(Path(__file__).resolve().parents[2]),
        ))
        try:
            worker = await asyncio.shield(launch)
        except BaseException:
            # A cancelled exec await can otherwise orphan a process being born.
            with anyio.CancelScope(shield=True):
                self._workers[index] = await launch
                await self._stop(index, propagate_cancel=False)
            raise
        self._workers[index] = worker
        try:
            if self._closed:
                raise RuntimeError("Piper closed during process launch")
            async with asyncio.timeout(60):
                kind, payload = await self._read(worker)
                if kind != 0 or payload:
                    raise RuntimeError("Piper startup failed")
        except BaseException:
            with anyio.CancelScope(shield=True):
                await self._stop(index, propagate_cancel=False)
            raise

    async def start(self):
        if not self._closed:
            return
        self._verify_model()
        self._closed = False
        try:
            for index in range(self.worker_count):
                await self._spawn(index)
                self._idle.put_nowait(index)
        except BaseException:
            with anyio.CancelScope(shield=True):
                await self.close()
            raise

    async def _stop(self, index, propagate_cancel=True):
        worker = self._workers[index]
        if worker is not None:
            if worker.returncode is None:
                try:
                    worker.kill()
                except ProcessLookupError:
                    pass
            # Inference cannot outlive admission ownership.
            reaped = asyncio.create_task(worker.wait())
            cancelled = False
            while not reaped.done():
                try:
                    await asyncio.shield(reaped)
                except asyncio.CancelledError:
                    # Reap first, then preserve cancellation on normal recovery.
                    # Cleanup finally callers already propagate their original
                    # exception and explicitly defer repeated cancellation.
                    cancelled = True
            reaped.result()
            self._workers[index] = None
            if cancelled and propagate_cancel:
                raise asyncio.CancelledError

    async def close(self):
        self._closed = True
        for index in range(len(self._workers)):
            await self._stop(index, propagate_cancel=False)
        while not self._idle.empty():
            self._idle.get_nowait()

    @staticmethod
    async def _read(worker):
        header = await worker.stdout.readexactly(5)
        kind, size = struct.unpack("!BI", header)
        if kind not in (0, 1, 2, 3):
            raise RuntimeError(f"Unknown Piper protocol kind: {kind}")
        if kind in (0, 2, 3):
            if size != 0:
                raise RuntimeError(f"Piper protocol kind {kind} must have zero payload, got {size}")
            return kind, b""
        if size == 0 or size > 16384:
            raise RuntimeError(f"Piper DATA payload size invalid: {size}")
        payload = await worker.stdout.readexactly(size)
        return kind, payload

    async def stream(self, text, voice=VOICE_ID, rate="+0%", pitch="+0Hz", timeout_seconds=8):
        if voice != VOICE_ID or int(pitch[:-2]) != 0:
            raise ValueError("Unsupported Piper voice/pitch")
        speed = 1 + int(rate[:-1]) / 100
        if not 0.75 <= speed <= 2:
            raise ValueError("Unsupported Piper rate")
        if self._closed:
            raise RuntimeError("Piper unavailable")

        loop = asyncio.get_running_loop()
        deadline = loop.time() + timeout_seconds

        def remaining() -> float:
            rem = deadline - loop.time()
            if rem <= 0:
                raise TimeoutError("Piper synthesis timed out")
            return rem

        index = await asyncio.wait_for(self._idle.get(), timeout=remaining())
        if self._closed:
            raise RuntimeError("Piper closed while waiting for a worker")
        healthy = False
        received_data = False
        total_bytes = 0

        try:
            worker = self._workers[index]
            if worker is None or worker.returncode is not None:
                await self._stop(index)
                if self._closed:
                    raise RuntimeError("Piper closed during recovery")
                await asyncio.wait_for(self._spawn(index), timeout=remaining())
                worker = self._workers[index]

            req_line = json.dumps({"text": text, "rate": speed}, ensure_ascii=False) + "\n"
            worker.stdin.write(req_line.encode("utf-8"))
            await asyncio.wait_for(worker.stdin.drain(), timeout=remaining())

            while True:
                kind, payload = await asyncio.wait_for(self._read(worker), timeout=remaining())
                if kind == 1:
                    received_data = True
                    total_bytes += len(payload)
                    if total_bytes > MAX_AUDIO_BYTES:
                        raise RuntimeError("Piper audio exceeds max limit")
                    yield payload
                elif kind == 3:
                    if not received_data:
                        raise RuntimeError("Piper sent END before any DATA")
                    healthy = True
                    break
                elif kind == 2:
                    raise RuntimeError("Piper synthesis error")
                else:
                    raise RuntimeError(f"Unexpected kind during stream: {kind}")
        finally:
            with anyio.CancelScope(shield=True):
                if not healthy:
                    await self._stop(index, propagate_cancel=False)
                if not self._closed:
                    self._idle.put_nowait(index)

    async def synthesize(self, text, voice=VOICE_ID, rate="+0%", pitch="+0Hz", timeout_seconds=8):
        collected = bytearray()
        async for chunk in self.stream(text, voice=voice, rate=rate, pitch=pitch, timeout_seconds=timeout_seconds):
            collected.extend(chunk)
            if len(collected) > MAX_AUDIO_BYTES:
                raise RuntimeError("Piper audio exceeds limit")
        if not collected:
            raise RuntimeError("Piper synthesis produced no audio")
        return bytes(collected)
