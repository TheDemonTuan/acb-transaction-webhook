"""Shared subprocess pool with framed binary IPC and cancellation shielding."""
import asyncio
import json
import os
from pathlib import Path
import struct
import sys
from typing import AsyncGenerator

import anyio

MAX_AUDIO_BYTES = 4 * 1024 * 1024


class ProcessTTSProvider:
    """Manages worker subprocesses communicating via a framed binary protocol."""

    def __init__(
        self,
        worker_module: str,
        worker_args: tuple[str, ...],
        worker_count: int = 1,
        worker_env: dict[str, str] | None = None,
        cwd: str | Path | None = None,
        startup_timeout: float = 60.0,
    ):
        if worker_count < 1:
            raise ValueError(f"worker_count must be at least 1, got {worker_count}")
        self.worker_module = worker_module
        self.worker_args = tuple(worker_args)
        self.worker_count = worker_count
        self.worker_env = dict(worker_env) if worker_env is not None else None
        self.cwd = str(cwd) if cwd is not None else str(Path(__file__).resolve().parents[2])
        self.startup_timeout = startup_timeout

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
    def available(self) -> bool:
        return not self._closed and all(
            worker is not None and worker.returncode is None for worker in self._workers
        )

    @staticmethod
    async def _read(worker) -> tuple[int, bytes]:
        header = await worker.stdout.readexactly(5)
        kind, size = struct.unpack("!BI", header)
        if kind not in (0, 1, 2, 3):
            raise RuntimeError(f"Unknown process protocol kind: {kind}")
        if kind in (0, 2, 3):
            if size != 0:
                raise RuntimeError(f"Process protocol kind {kind} must have zero payload, got {size}")
            return kind, b""
        if size == 0 or size > 16384:
            raise RuntimeError(f"Process DATA payload size invalid: {size}")
        payload = await worker.stdout.readexactly(size)
        return kind, payload

    async def _spawn(self, index: int) -> None:
        if self._closed:
            raise RuntimeError("Process provider is closed")
        base_env = dict(os.environ)
        if self.worker_env:
            base_env.update(self.worker_env)

        cmd = [sys.executable, "-m", self.worker_module, *self.worker_args]
        launch = asyncio.create_task(
            asyncio.create_subprocess_exec(
                *cmd,
                stdin=asyncio.subprocess.PIPE,
                stdout=asyncio.subprocess.PIPE,
                stderr=None,
                env=base_env,
                cwd=self.cwd,
            )
        )
        try:
            worker = await asyncio.shield(launch)
        except BaseException:
            with anyio.CancelScope(shield=True):
                self._workers[index] = await launch
                await self._stop(index, propagate_cancel=False)
            raise

        self._workers[index] = worker
        try:
            if self._closed:
                raise RuntimeError("Process provider closed during process launch")
            async with asyncio.timeout(self.startup_timeout):
                kind, payload = await self._read(worker)
                if kind != 0 or payload:
                    raise RuntimeError("Process worker startup failed (invalid READY frame)")
        except BaseException:
            with anyio.CancelScope(shield=True):
                await self._stop(index, propagate_cancel=False)
            raise

    async def _stop(self, index: int, propagate_cancel: bool = True) -> None:
        worker = self._workers[index]
        if worker is not None:
            if getattr(worker, "returncode", None) is None:
                kill_fn = getattr(worker, "kill", None)
                if callable(kill_fn):
                    try:
                        kill_fn()
                    except ProcessLookupError:
                        pass
            wait_fn = getattr(worker, "wait", None)
            if callable(wait_fn):
                reaped = asyncio.create_task(wait_fn())
                cancelled = False
                while not reaped.done():
                    try:
                        await asyncio.shield(reaped)
                    except asyncio.CancelledError:
                        cancelled = True
                reaped.result()
            self._workers[index] = None
            if cancelled and propagate_cancel:
                raise asyncio.CancelledError
    async def start(self) -> None:
        if not self._closed:
            return
        self._closed = False
        try:
            for index in range(self.worker_count):
                await self._spawn(index)
                self._idle.put_nowait(index)
        except BaseException:
            with anyio.CancelScope(shield=True):
                await self.close()
            raise

    async def close(self) -> None:
        self._closed = True
        for index in range(len(self._workers)):
            await self._stop(index, propagate_cancel=False)
        while not self._idle.empty():
            self._idle.get_nowait()

    async def stream_request(
        self,
        request: dict,
        timeout_seconds: float = 8.0,
        spawn_fn=None,
        stop_fn=None,
    ) -> AsyncGenerator[bytes, None]:
        if self._closed:
            raise RuntimeError("Process provider unavailable")

        loop = asyncio.get_running_loop()
        deadline = loop.time() + timeout_seconds

        def remaining() -> float:
            rem = deadline - loop.time()
            if rem <= 0:
                raise TimeoutError("Synthesis timed out")
            return rem

        index = await asyncio.wait_for(self._idle.get(), timeout=remaining())
        if self._closed:
            raise RuntimeError("Process provider closed while waiting for worker")

        healthy = False
        received_data = False
        total_bytes = 0

        try:
            stop_impl = stop_fn or self._stop
            spawn_impl = spawn_fn or self._spawn
            worker = self._workers[index]
            if worker is None or getattr(worker, "returncode", None) is not None:
                await stop_impl(index)
                if self._closed:
                    raise RuntimeError("Process provider closed during recovery")
                await asyncio.wait_for(spawn_impl(index), timeout=remaining())
                worker = self._workers[index]

            req_line = json.dumps(request, ensure_ascii=False) + "\n"
            worker.stdin.write(req_line.encode("utf-8"))
            await asyncio.wait_for(worker.stdin.drain(), timeout=remaining())

            while True:
                kind, payload = await asyncio.wait_for(self._read(worker), timeout=remaining())
                if kind == 1:
                    received_data = True
                    total_bytes += len(payload)
                    if total_bytes > MAX_AUDIO_BYTES:
                        raise RuntimeError("Audio exceeds max limit")
                    yield payload
                elif kind == 3:
                    if not received_data:
                        raise RuntimeError("Process worker sent END before any DATA")
                    healthy = True
                    break
                elif kind == 2:
                    raise RuntimeError("Process worker synthesis error")
                else:
                    raise RuntimeError(f"Unexpected kind during stream: {kind}")
        finally:
            with anyio.CancelScope(shield=True):
                if not healthy:
                    await stop_impl(index, propagate_cancel=False)
                if not self._closed:
                    self._idle.put_nowait(index)
