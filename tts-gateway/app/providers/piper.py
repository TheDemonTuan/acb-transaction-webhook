"""Piper TTS provider delegating transport to ProcessTTSProvider."""
import hashlib
from pathlib import Path

from .process import ProcessTTSProvider, MAX_AUDIO_BYTES

VOICE_ID = "vi-VN-vais1000-medium"
MODEL_SHA256 = "ec7c89e2c85f4d1edc24b6120c18aaf1bda614f06b511567eb9c7c0de15e2dab"
CONFIG_SHA256 = "fafb9da1354ed4b77c31af228ed41fb41cd825c14cffa105454b25e6ae751ee0"
DEFAULT_MODEL = Path(__file__).resolve().parents[2] / "assets" / "models" / (VOICE_ID + ".onnx")


class PiperTTSProvider:
    def __init__(self, model_path=DEFAULT_MODEL, worker_count: int = 2):
        if worker_count not in (1, 2):
            raise ValueError(f"worker_count must be 1 or 2, got {worker_count}")
        self.model_path = Path(model_path)
        self.worker_count = worker_count
        env = {"OMP_NUM_THREADS": "1", "OPENBLAS_NUM_THREADS": "1", "MKL_NUM_THREADS": "1"}
        self._process_pool = ProcessTTSProvider(
            worker_module="app.providers.piper_worker",
            worker_args=(str(self.model_path),),
            worker_count=worker_count,
            worker_env=env,
            startup_timeout=60.0,
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

    def _verify_model(self):
        for path, digest in ((self.model_path, MODEL_SHA256), (Path(str(self.model_path) + ".json"), CONFIG_SHA256)):
            hasher = hashlib.sha256()
            with path.open("rb") as source:
                for chunk in iter(lambda: source.read(1024 * 1024), b""):
                    hasher.update(chunk)
            if hasher.hexdigest() != digest:
                raise RuntimeError("Fixed Piper model checksum mismatch")

    async def start(self):
        self._verify_model()
        await self._process_pool.start()

    async def close(self):
        await self._process_pool.close()
    async def stream(self, text, voice=VOICE_ID, rate="+0%", pitch="+0Hz", timeout_seconds=8):
        if voice != VOICE_ID or int(pitch[:-2]) != 0:
            raise ValueError("Unsupported Piper voice/pitch")
        speed = 1 + int(rate[:-1]) / 100
        if not 0.75 <= speed <= 2:
            raise ValueError("Unsupported Piper rate")
        request = {"text": text, "rate": speed}
        async for chunk in self._process_pool.stream_request(
            request,
            timeout_seconds=timeout_seconds,
            spawn_fn=self._spawn,
            stop_fn=self._stop,
        ):
            yield chunk
    async def synthesize(self, text, voice=VOICE_ID, rate="+0%", pitch="+0Hz", timeout_seconds=8):
        collected = bytearray()
        async for chunk in self.stream(text, voice=voice, rate=rate, pitch=pitch, timeout_seconds=timeout_seconds):
            collected.extend(chunk)
            if len(collected) > MAX_AUDIO_BYTES:
                raise RuntimeError("Piper audio exceeds limit")
        if not collected:
            raise RuntimeError("Piper synthesis produced no audio")
        return bytes(collected)
