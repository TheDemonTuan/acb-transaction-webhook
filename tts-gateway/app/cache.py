"""In-memory cache with TTL, byte limit, and single-flight coalescing."""
import asyncio
import hashlib
import time
from typing import Optional, Dict, Tuple

class TTSCache:
    def __init__(
        self,
        max_items: int = 256,
        max_bytes: int = 32 * 1024 * 1024,
        ttl_seconds: int = 600,
    ):
        self.max_items = max_items
        self.max_bytes = max_bytes
        self.ttl_seconds = ttl_seconds
        # key -> (audio_bytes, timestamp, provider, voice)
        self._cache: Dict[str, Tuple[bytes, float, str, str]] = {}
        self._current_bytes: int = 0
        # key -> asyncio.Future
        self._in_flight: Dict[str, asyncio.Future] = {}
        self._lock = asyncio.Lock()

    @staticmethod
    def make_key(
        text: str,
        voice: str,
        rate: str,
        pitch: str,
    ) -> str:
        raw = f"VIENEU_ONLY|{text}|{voice}|{rate}|{pitch}"
        return hashlib.sha256(raw.encode("utf-8")).hexdigest()

    async def get(self, key: str) -> Optional[Tuple[bytes, str, str]]:
        async with self._lock:
            if key in self._cache:
                data, ts, provider, voice = self._cache[key]
                if time.time() - ts <= self.ttl_seconds:
                    return data, provider, voice
                else:
                    self._current_bytes -= len(data)
                    del self._cache[key]
            return None

    async def set(self, key: str, data: bytes, provider: str, voice: str):
        async with self._lock:
            now = time.time()
            data_len = len(data)
            if data_len > self.max_bytes:
                return  # Payload exceeds total cache capacity

            # Adjust if updating existing entry
            if key in self._cache:
                self._current_bytes -= len(self._cache[key][0])
                del self._cache[key]

            # Evict oldest entries until within max_items and max_bytes
            while self._cache and (
                len(self._cache) >= self.max_items
                or self._current_bytes + data_len > self.max_bytes
            ):
                oldest_key = min(self._cache.keys(), key=lambda k: self._cache[k][1])
                self._current_bytes -= len(self._cache[oldest_key][0])
                del self._cache[oldest_key]

            self._cache[key] = (data, now, provider, voice)
            self._current_bytes += data_len

    async def get_or_compute(self, key: str, compute_func):
        cached = await self.get(key)
        if cached:
            return cached

        future = None
        should_compute = False

        async with self._lock:
            if key in self._in_flight:
                future = self._in_flight[key]
            else:
                loop = asyncio.get_running_loop()
                future = loop.create_future()
                self._in_flight[key] = future
                should_compute = True

        if should_compute:
            try:
                result = await compute_func()
                audio_bytes, provider, voice = result
                await self.set(key, audio_bytes, provider, voice)
                future.set_result(result)
                return result
            except Exception as e:
                future.set_exception(e)
                raise
            finally:
                async with self._lock:
                    self._in_flight.pop(key, None)
        else:
            return await future
