"""Shared subprocess pool with framed binary IPC and cancellation shielding."""
import asyncio
import json
import logging
import os
from pathlib import Path
import struct
import sys
from typing import AsyncGenerator

import anyio

MAX_AUDIO_BYTES = 4 * 1024 * 1024
logger = logging.getLogger(__name__)


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
        self._maintenance: set[asyncio.Task] = set()

    @property
    def idle_count(self) -> int:
        return self._idle.qsize()

    @property
    def active_count(self) -> int:
        return self.worker_count - self._idle.qsize()

    @property
    def available(self) -> bool:
        return not self._closed and not self._maintenance and all(
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
        """Kill, close all pipes, and reap before releasing the worker slot."""
        worker = self._workers[index]
        if worker is None:
            return
        if worker.returncode is None:
            try:
                worker.kill()
            except ProcessLookupError:
                pass

        async def cleanup() -> None:
            # communicate closes stdin and drains stdout so a full pipe cannot
            # stall reaping. An exited process can still have pending pipe closes.
            await worker.communicate()
            if worker.stdin is not None:
                try:
                    await worker.stdin.wait_closed()
                except (BrokenPipeError, ConnectionResetError):
                    # The killed worker may have closed its read end first.
                    pass

        reaped = asyncio.create_task(cleanup())
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

    async def _replace(self, index: int, spawn_impl) -> None:
        """Warm a replacement independently of the failed request's deadline."""
        try:
            await spawn_impl(index)
        except Exception:
            logger.exception("Process worker replacement failed for slot %s", index)
        finally:
            if not self._closed:
                self._idle.put_nowait(index)

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
        # A replacement owns its slot until READY or failed startup cleanup.
        # Join cancellation before stopping workers so pipe readers never race.
        maintenance = tuple(self._maintenance)
        for task in maintenance:
            task.cancel()
        if maintenance:
            joined = asyncio.gather(*maintenance, return_exceptions=True)
            while not joined.done():
                try:
                    await asyncio.shield(joined)
                except asyncio.CancelledError:
                    pass
            joined.result()
            self._maintenance.difference_update(maintenance)
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
        inference_started = False
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

            inference_started = True
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
                    if not healthy and inference_started:
                        # Cleanup above must reap the old worker before either
                        # releasing admission or starting its replacement.
                        task = asyncio.create_task(self._replace(index, spawn_impl))
                        self._maintenance.add(task)
                        task.add_done_callback(self._maintenance.discard)
                    else:
                        self._idle.put_nowait(index)
