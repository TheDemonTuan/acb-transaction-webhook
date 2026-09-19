"""Microsoft Edge TTS provider."""
import asyncio
import io
from typing import AsyncGenerator, Optional
import edge_tts

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
            voice = "vi-VN-HoaiMyNeural"

        communicate = edge_tts.Communicate(
            text=text,
            voice=voice,
            rate=rate,
            pitch=pitch,
        )
        has_audio = False
        async for chunk in communicate.stream():
            if chunk.get("type") == "audio" and "data" in chunk and chunk["data"]:
                has_audio = True
                yield chunk["data"]

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
        async def _do_synth() -> bytes:
            buffer = io.BytesIO()
            async for chunk in self.stream(text=text, voice=voice, rate=rate, pitch=pitch):
                buffer.write(chunk)
            return buffer.getvalue()

        return await asyncio.wait_for(_do_synth(), timeout=timeout_seconds)
