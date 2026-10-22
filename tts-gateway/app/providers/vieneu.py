"""VieNeu v3 Turbo TTS provider wrapper running single-process with ProcessTTSProvider."""
import os
from pathlib import Path
from typing import AsyncGenerator

from .process import ProcessTTSProvider, MAX_AUDIO_BYTES

VOICE_ID = "vi-VN-vieneu-v3-turbo-mai-anh-v1"
DEFAULT_MODEL_ROOT = Path(__file__).resolve().parents[2] / "assets" / "models" / "vieneu"


class VieneuTTSProvider:
    def __init__(self, model_root: str | Path = DEFAULT_MODEL_ROOT, worker_count: int = 1, threads: int = 2):
        if worker_count != 1:
            raise ValueError(f"VieneuTTSProvider only supports 1 worker process, got {worker_count}")
        if not (1 <= threads <= 4):
            raise ValueError(f"threads must be between 1 and 4, got {threads}")

        self.model_root = Path(model_root).resolve()
        self.threads = threads
        self.worker_count = 1
        hf_home = str(self.model_root / "hf")
        worker_env = {
            "HF_HOME": hf_home,
            "HF_HUB_CACHE": str(self.model_root / "hf" / "hub"),
            "HF_HUB_OFFLINE": "1",
            "HF_HUB_DISABLE_TELEMETRY": "1",
            "OMP_NUM_THREADS": str(threads),
            "OPENBLAS_NUM_THREADS": str(threads),
            "MKL_NUM_THREADS": str(threads),
        }

        self._process_pool = ProcessTTSProvider(
            worker_module="app.providers.vieneu_worker",
            worker_args=(str(self.model_root), str(self.threads)),
            worker_count=1,
            worker_env=worker_env,
            startup_timeout=120.0,
        )

    @property
    def idle_count(self) -> int:
        return self._process_pool.idle_count

    @property
    def active_count(self) -> int:
        return self._process_pool.active_count

    @property
    def available(self) -> bool:
        return self._process_pool.available

    @property
    def _closed(self) -> bool:
        return self._process_pool._closed

    @_closed.setter
    def _closed(self, value: bool) -> None:
        self._process_pool._closed = value

    @property
    def _workers(self):
        return self._process_pool._workers

    @property
    def _idle(self):
        return self._process_pool._idle

    async def _spawn(self, index: int):
        return await self._process_pool._spawn(index)

    async def _stop(self, index: int, propagate_cancel: bool = True):
        return await self._process_pool._stop(index, propagate_cancel=propagate_cancel)

    async def start(self) -> None:
        await self._process_pool.start()

    async def close(self) -> None:
        await self._process_pool.close()

    async def stream(
        self,
        text: str,
        voice: str = VOICE_ID,
        rate: str = "+0%",
        pitch: str = "+0Hz",
        timeout_seconds: float = 8.0,
    ) -> AsyncGenerator[bytes, None]:
        if voice != VOICE_ID:
            raise ValueError(f"Unsupported VieNeu voice: {voice}")
        if rate != "+0%":
            raise ValueError(f"Unsupported VieNeu rate: {rate}. Only natural speed (+0%) is supported.")
        if pitch != "+0Hz":
            raise ValueError(f"Unsupported VieNeu pitch: {pitch}. Only natural pitch (+0Hz) is supported.")

        request = {"text": text, "voice": voice, "rate": rate, "pitch": pitch}
        async for chunk in self._process_pool.stream_request(
            request,
            timeout_seconds=timeout_seconds,
            spawn_fn=self._spawn,
            stop_fn=self._stop,
        ):
            yield chunk

    async def synthesize(
        self,
        text: str,
        voice: str = VOICE_ID,
        rate: str = "+0%",
        pitch: str = "+0Hz",
        timeout_seconds: float = 8.0,
    ) -> bytes:
        collected = bytearray()
        async for chunk in self.stream(text, voice=voice, rate=rate, pitch=pitch, timeout_seconds=timeout_seconds):
            collected.extend(chunk)
            if len(collected) > MAX_AUDIO_BYTES:
                raise RuntimeError("VieNeu audio exceeds max limit")
        if not collected:
            raise RuntimeError("VieNeu synthesis produced no audio")
        return bytes(collected)
