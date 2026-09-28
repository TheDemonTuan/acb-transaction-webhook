"""Google TTS (gTTS) fallback provider."""
import asyncio
import io

from gtts import gTTS


async def _apply_rate(audio: bytes, rate: str, timeout_seconds: float) -> bytes:
    digits = rate[:-1]
    if not (rate.endswith("%") and 2 <= len(rate) <= 5 and (digits.isdigit() or (digits[0] in "+-" and digits[1:].isdigit()))):
        raise ValueError(f"GTTS_RATE_PROCESSING_FAILED: invalid rate {rate!r}")
    percent = int(digits)
    if not -50 <= percent <= 100:
        raise ValueError(f"GTTS_RATE_PROCESSING_FAILED: invalid rate {rate!r}")
    if percent == 0:
        return audio

    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout_seconds
    try:
        process = await asyncio.wait_for(asyncio.create_subprocess_exec(
            "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
            "-threads", "1", "-protocol_whitelist", "pipe", "-f", "mp3",
            "-i", "pipe:0", "-vn", "-af", f"atempo={1 + percent / 100:g}",
            "-codec:a", "libmp3lame", "-f", "mp3", "pipe:1",
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        ), max(0, deadline - loop.time()))
    except OSError as exc:
        raise RuntimeError("GTTS_RATE_PROCESSING_FAILED: ffmpeg unavailable") from exc

    try:
        stdout, stderr = await asyncio.wait_for(process.communicate(audio), max(0, deadline - loop.time()))
    except BaseException:
        if process.returncode is None:
            process.kill()
        await process.communicate()
        raise
    if process.returncode != 0 or not stdout:
        raise RuntimeError(f"GTTS_RATE_PROCESSING_FAILED: {stderr.decode(errors='replace').strip() or 'empty audio'}")
    return stdout

class GTTSProvider:
    name: str = "gtts"

    async def synthesize(
        self,
        text: str,
        voice: str = "vi",
        rate: str = "+0%",
        pitch: str = "+0Hz",
        timeout_seconds: float = 3.0,
    ) -> bytes:
        loop = asyncio.get_running_loop()
        deadline = loop.time() + timeout_seconds

        def _run_gtts() -> bytes:
            tts = gTTS(
                text=text,
                lang="vi",
                slow=False,
                lang_check=False,
                timeout=max(1, int(timeout_seconds)),
            )
            fp = io.BytesIO()
            tts.write_to_fp(fp)
            return fp.getvalue()

        try:
            data = await asyncio.wait_for(
                asyncio.to_thread(_run_gtts),
                timeout=max(0, deadline - loop.time()),
            )
            if not data:
                raise ValueError("GTTS_NO_AUDIO: empty audio received")
            return await _apply_rate(data, rate, max(0, deadline - loop.time()))
        except asyncio.TimeoutError:
            raise TimeoutError(f"GTTS_TIMEOUT: synthesis timed out after {timeout_seconds}s")
