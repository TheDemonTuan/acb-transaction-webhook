"""Microsoft Edge TTS provider."""
import asyncio
from typing import AsyncGenerator
import edge_tts
from app.config import config

SUPPORTED_VOICES = {
    "vi-VN-HoaiMyNeural": "Hoài My",
    "vi-VN-NamMinhNeural": "Nam Minh",
}

class EdgeTTSProvider:
    name: str = "edge"

    async def stream(
        self,
        text: str,
        voice: str = "vi-VN-HoaiMyNeural",
        rate: str = "+0%",
        pitch: str = "+0Hz",
    ) -> AsyncGenerator[bytes, None]:
        if voice not in SUPPORTED_VOICES:
            raise ValueError("Unsupported fixed Edge voice")

        communicate = edge_tts.Communicate(
            text=text,
            voice=voice,
            rate=rate,
            pitch=pitch,
        )
        has_audio = False
        upstream = communicate.stream()
        try:
            async for chunk in upstream:
                if chunk.get("type") == "audio" and "data" in chunk and chunk["data"]:
                    has_audio = True
                    yield chunk["data"]
        finally:
            await upstream.aclose()

        if not has_audio:
            raise ValueError("EDGE_NO_AUDIO: no audio chunks received")

    async def synthesize(
        self,
        text: str,
        voice: str = "vi-VN-HoaiMyNeural",
        rate: str = "+0%",
        pitch: str = "+0Hz",
        timeout_seconds: float = 4.0,
    ) -> bytes:
        buffer = bytearray()
        generator = self.stream(text=text, voice=voice, rate=rate, pitch=pitch)
        deadline = asyncio.get_running_loop().time() + timeout_seconds
        first = True
        try:
            async with asyncio.timeout_at(deadline):
                while True:
                    stage_limit = config.edge_stream_initial_timeout if first else config.edge_stream_idle_timeout
                    budget = min(stage_limit, deadline - asyncio.get_running_loop().time())
                    if budget <= 0:
                        raise TimeoutError
                    try:
                        chunk = await asyncio.wait_for(anext(generator), budget)
                    except StopAsyncIteration:
                        break
                    first = False
                    if len(buffer) + len(chunk) > 4 * 1024 * 1024:
                        raise ValueError("TTS_AUDIO_TOO_LARGE")
                    buffer.extend(chunk)
                return bytes(buffer)
        finally:
            buffer.clear()
            await generator.aclose()
