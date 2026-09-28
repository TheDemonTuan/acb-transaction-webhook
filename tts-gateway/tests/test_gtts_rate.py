import asyncio
from array import array
from math import cos, pi, sin
import shutil
import subprocess
import sys

import pytest
from httpx import AsyncClient, ASGITransport
from app.main import app, edge_provider, edge_circuit

from app.providers.gtts import GTTSProvider, _apply_rate


def tone():
    return subprocess.run(
        ["ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi",
         "-i", "sine=frequency=440:duration=2", "-codec:a", "libmp3lame",
         "-f", "mp3", "pipe:1"], capture_output=True, check=True,
    ).stdout


def decoded_samples(audio):
    pcm = subprocess.run(
        ["ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "mp3",
         "-i", "pipe:0", "-f", "s16le", "-ac", "1", "-ar", "8000", "pipe:1"],
        input=audio, capture_output=True, check=True,
    ).stdout
    samples = array("h")
    samples.frombytes(pcm)
    return samples


@pytest.mark.asyncio
@pytest.mark.parametrize("rate,expected", [("+0%", 2), ("+25%", 1.6), ("+100%", 1), ("-25%", 2.667)])
async def test_tempo_changes_duration_without_changing_pitch(rate, expected):
    if not shutil.which("ffmpeg") or not shutil.which("ffprobe"):
        pytest.fail("FFmpeg and ffprobe are required for real tempo verification")
    audio = tone()
    result = await _apply_rate(audio, rate, 5)
    samples = decoded_samples(result)
    assert abs(len(samples) / 8000 - expected) < 0.12
    if rate == "+0%":
        assert result is audio
    else:
        # Measure the dominant fundamental frequency on decoded PCM, not MP3 metadata.
        segment = samples[1600:2400]
        power = lambda hz: abs(sum(v * complex(cos(2*pi*hz*i/8000), sin(2*pi*hz*i/8000)) for i, v in enumerate(segment)))
        assert 419 <= max(range(300, 551), key=power) <= 461


@pytest.mark.asyncio
async def test_invalid_rate_and_failed_transcode():
    for rate in ("-51%", "+101%", "1.25", "0%;evil", "-100%"):
        with pytest.raises(ValueError, match="GTTS_RATE_PROCESSING_FAILED"):
            await _apply_rate(b"audio", rate, 1)
    with pytest.raises(RuntimeError, match="GTTS_RATE_PROCESSING_FAILED"):
        await _apply_rate(b"not mp3", "+25%", 2)


@pytest.mark.asyncio
async def test_empty_transport_rejected(monkeypatch):
    class EmptyTTS:
        def __init__(self, **kwargs):
            pass

        def write_to_fp(self, fp):
            pass

    monkeypatch.setattr("app.providers.gtts.gTTS", EmptyTTS)
    with pytest.raises(ValueError, match="GTTS_NO_AUDIO"):
        await GTTSProvider().synthesize("test", rate="+25%")

@pytest.mark.asyncio
@pytest.mark.parametrize("cancel", [False, True])
async def test_tempo_stops_child_on_timeout_or_cancel(monkeypatch, cancel):
    spawn = asyncio.create_subprocess_exec
    children = []

    async def blocked_encoder(*args, **kwargs):
        child = await spawn(sys.executable, "-c", "import time; time.sleep(60)", **kwargs)
        children.append(child)
        return child

    monkeypatch.setattr("app.providers.gtts.asyncio.create_subprocess_exec", blocked_encoder)
    if cancel:
        task = asyncio.create_task(_apply_rate(b"audio", "+25%", 1))
        while not children:
            await asyncio.sleep(0.01)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
    else:
        with pytest.raises(asyncio.TimeoutError):
            await _apply_rate(b"audio", "+25%", 0.1)
    assert children and children[0].returncode is not None

@pytest.mark.asyncio
async def test_fallback_endpoints_cache_tempo_and_initial_timeout(monkeypatch):
    if not shutil.which("ffmpeg"):
        pytest.fail("FFmpeg is required for real fallback verification")
    source = tone()
    calls = []

    class ToneTTS:
        def __init__(self, **kwargs):
            calls.append(kwargs)

        def write_to_fp(self, fp):
            fp.write(source)

    monkeypatch.setattr("app.providers.gtts.gTTS", ToneTTS)
    monkeypatch.setattr(edge_circuit, "can_attempt", lambda: False)
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        for endpoint in ("/synthesize", "/synthesize/stream"):
            for rate, expected in (("+0%", 2), ("+25%", 1.6)):
                body = {"text": f"rate fixture {endpoint} {rate}", "rate": rate}
                first = await client.post(endpoint, json=body)
                second = await client.post(endpoint, json=body)
                assert first.status_code == second.status_code == 200
                assert first.headers["x-tts-provider"] == "gtts"
                assert first.headers["x-tts-cached"] == "false"
                assert second.headers["x-tts-cached"] == "true"
                assert abs(len(decoded_samples(first.content)) / 8000 - expected) < 0.12
                assert second.content == first.content

        monkeypatch.setattr(edge_circuit, "can_attempt", lambda: True)

        async def delayed_edge(**kwargs):
            await asyncio.sleep(0.2)
            yield source

        from app.config import config
        monkeypatch.setattr(config, "edge_stream_initial_timeout", 0.01)
        monkeypatch.setattr(edge_provider, "stream", delayed_edge)
        response = await client.post("/synthesize/stream", json={
            "text": "edge initial timeout fixture", "rate": "+100%", "cacheable": False,
        })
        assert response.status_code == 200
        assert response.headers["x-tts-provider"] == "gtts"
        assert abs(len(decoded_samples(response.content)) / 8000 - 1) < 0.12
        async def synthesized_edge(**kwargs):
            return source

        monkeypatch.setattr(edge_provider, "synthesize", synthesized_edge)
        edge = await client.post("/synthesize", json={
            "text": "edge preprocessed fixture", "rate": "+25%", "cacheable": False,
        })
        assert edge.status_code == 200
        assert edge.headers["x-tts-provider"] == "edge"
        assert edge.content == source
    assert len(calls) == 5
