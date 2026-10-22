"""Real local model/encoder coverage; no upstream calls or fabricated audio."""
import asyncio
import hashlib
import os
import signal
import shutil
import subprocess

import pytest
from httpx import ASGITransport, AsyncClient
from pydantic import ValidationError

from app.assets import InvalidPack, synthesis_key
from app.admission import AdmissionController
from app.providers.piper import PiperTTSProvider, VOICE_ID
from app.schemas import SynthesizeRequest


def is_mp3(audio):
    return any(audio[index] == 255 and audio[index + 1] & 224 == 224
               and (audio[index + 2] >> 4) not in (0, 15)
               for index in range(min(len(audio) - 2, 4096)))


def test_piper_fixed_contract_and_synthesis_key():
    request = SynthesizeRequest(text="Đã nhận năm trăm nghìn đồng.")
    assert request.voice == VOICE_ID and request.provider_mode == "PIPER_ONLY"
    canonical = "piper|vi-VN-vais1000-medium|1.25|0|money-vi-v1|pack-schema-2"
    assert synthesis_key(VOICE_ID, 1.25, 0) == hashlib.sha256(canonical.encode()).hexdigest()
    with pytest.raises(InvalidPack):
        synthesis_key(VOICE_ID, 1.25, 1)
    for overrides in ({"pitch": "+1Hz"}, {"voice": "vi-VN-NamMinhNeural"},
                      {"provider_mode": "EDGE_ONLY"}, {"rate": "-26%"}, {"rate": "+101%"}):
        with pytest.raises(ValidationError):
            SynthesizeRequest(text="Đã nhận tiền.", **overrides)
    assert SynthesizeRequest(text="Đã nhận tiền.", voice="vi-VN-NamMinhNeural", provider_mode="EDGE_ONLY").voice == "vi-VN-NamMinhNeural"


@pytest.mark.asyncio
async def test_real_piper_mp3_and_persistent_workers():
    provider = PiperTTSProvider()
    try:
        await provider.start()
        processes = tuple(provider._workers)
        assert provider.available and len(processes) == 2
        text = "Đã nhận năm trăm nghìn đồng. Cảm ơn bạn đã ghé quầy."
        slow = await provider.synthesize(text, rate="-25%")
        fast = await provider.synthesize(text, rate="+100%")
        assert is_mp3(slow) and is_mp3(fast)
        assert tuple(provider._workers) == processes
        streamed = b"".join([chunk async for chunk in provider.stream(text, rate="+25%")])
        assert is_mp3(streamed) and len(streamed) <= 4 * 1024 * 1024
    finally:
        await provider.close()
    assert all(process.returncode is not None for process in processes)
    assert not provider.available


@pytest.mark.asyncio
@pytest.mark.skipif(not shutil.which("ffmpeg"), reason="Build-only FFmpeg required for actual PCM duration measurement")
async def test_real_piper_rate_changes_decoded_duration_direction():
    provider = PiperTTSProvider()
    def decoded_duration(audio):
        decoded = subprocess.run([
            "ffmpeg", "-nostdin", "-v", "error", "-f", "mp3", "-i", "pipe:0",
            "-f", "s16le", "-ac", "1", "-ar", "24000", "pipe:1",
        ], input=audio, capture_output=True, check=True, timeout=30)
        assert decoded.stdout
        return len(decoded.stdout) / (24000 * 2)
    try:
        await provider.start()
        text = "Đã nhận năm trăm nghìn đồng. Cảm ơn bạn đã ghé quầy."
        slow = await provider.synthesize(text, rate="-25%")
        fast = await provider.synthesize(text, rate="+100%")
        # Piper duration prediction is stochastic; measure decoded PCM direction,
        # not an invented byte-size or exact duration-ratio threshold.
        assert decoded_duration(slow) > decoded_duration(fast)
    finally:
        await provider.close()


@pytest.mark.asyncio
@pytest.mark.skipif(os.name != "posix", reason="SIGSTOP fault injection requires POSIX; Docker smoke covers deployment")
async def test_cancellation_reaps_real_inference_before_releasing_admission():
    provider = PiperTTSProvider()
    controller = AdmissionController()
    try:
        await provider.start()
        process = provider._workers[0]
        os.kill(process.pid, signal.SIGSTOP)
        task = asyncio.create_task(controller.run_singleflight(
            "real-stopped-worker", lambda: provider.synthesize("Đã nhận năm trăm nghìn đồng."), 8))
        while provider._idle.qsize() == 2:
            await asyncio.sleep(0)
        assert controller.active_count == 1 and process.returncode is None
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert process.returncode is not None
        assert controller.active_count == 0 and controller.total_waiters == 0
        audio = await provider.synthesize("Đã nhận năm trăm nghìn đồng.")
        assert is_mp3(audio)
        # Consume/reload the killed worker on the next turn; still only two slots.
        assert is_mp3(await provider.synthesize("Đã nhận một triệu đồng."))
        assert provider.available and len(provider._workers) == 2
    finally:
        await provider.close()


@pytest.mark.asyncio
async def test_real_piper_http_buffer_stream_and_same_voice(monkeypatch):
    import app.main as gateway
    from app.config import config
    monkeypatch.setattr(config, "require_auth", False)
    monkeypatch.setattr(config, "internal_token", "")
    monkeypatch.setattr(config, "token_file", "")
    provider = PiperTTSProvider()
    monkeypatch.setattr(gateway, "piper_provider", provider)
    async with gateway.lifespan(gateway.app):
        assert provider.available
        async with AsyncClient(transport=ASGITransport(app=gateway.app), base_url="http://test") as client:
            health = (await client.get("/health")).json()
            assert health["primary"] == "vieneu" and health["piper"]["available"]
            for route in ("/synthesize", "/synthesize/stream"):
                response = await client.post(route, json={"text": "Đã nhận năm trăm nghìn đồng.", "cacheable": False})
                assert response.status_code == 200
                assert response.headers["x-tts-provider"] == "piper"
                assert response.headers["x-tts-voice"] == VOICE_ID
                assert response.headers["x-tts-fallback"] == "false"
                assert is_mp3(response.content)
    assert not provider.available


@pytest.mark.asyncio
async def test_unavailable_piper_never_calls_edge(monkeypatch):
    import app.main as gateway
    from app.config import config
    monkeypatch.setattr(config, "require_auth", False)
    monkeypatch.setattr(config, "internal_token", "")
    monkeypatch.setattr(config, "token_file", "")
    monkeypatch.setattr(gateway, "piper_provider", PiperTTSProvider())
    async def forbidden(*args, **kwargs):
        pytest.fail("Piper must never fall back to Edge")
    monkeypatch.setattr(gateway.edge_provider, "synthesize", forbidden)
    async with AsyncClient(transport=ASGITransport(app=gateway.app), base_url="http://test") as client:
        response = await client.post("/synthesize", json={"text": "Không có mô hình.", "cacheable": False})
        assert response.status_code == 502 and response.json()["detail"] == "TTS_UNAVAILABLE"


@pytest.mark.asyncio
@pytest.mark.skipif(os.name != "posix", reason="Stopped-inference fault requires SIGSTOP")
async def test_real_http_disconnect_kills_inference(monkeypatch):
    import app.main as gateway
    from app.config import config
    from starlette.requests import Request
    monkeypatch.setattr(config, "require_auth", False)
    monkeypatch.setattr(config, "internal_token", "")
    monkeypatch.setattr(config, "token_file", "")
    provider = PiperTTSProvider()
    controller = AdmissionController()
    monkeypatch.setattr(gateway, "piper_provider", provider)
    monkeypatch.setattr(gateway, "admission", controller)
    disconnected = asyncio.Event()
    async def receive():
        await disconnected.wait()
        return {"type": "http.disconnect"}
    request = Request({"type": "http", "method": "POST", "path": "/synthesize"}, receive)
    try:
        await provider.start()
        process = provider._workers[0]
        os.kill(process.pid, signal.SIGSTOP)
        task = asyncio.create_task(gateway.synthesize(
            SynthesizeRequest(text="Đã nhận năm trăm nghìn đồng.", cacheable=False), request=request))
        while provider._idle.qsize() == 2:
            await asyncio.sleep(0)
        disconnected.set()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert process.returncode is not None
        assert controller.active_count == controller.total_waiters == 0
    finally:
        await provider.close()


@pytest.mark.asyncio
async def test_consumer_cancellation_during_recovery_reaps_without_respawn(monkeypatch):
    provider = PiperTTSProvider()
    controller = AdmissionController()
    reap_started = asyncio.Event()
    release_reap = asyncio.Event()
    producer_cancelled = asyncio.Event()
    task = None
    try:
        await provider.start()
        process = provider._workers[0]
        process.kill()
        await process.wait()
        original_wait = process.wait
        async def delayed_reap():
            # Delay only the OS-wait completion boundary of a real exited worker.
            reap_started.set()
            await release_reap.wait()
            return await original_wait()
        monkeypatch.setattr(process, "wait", delayed_reap)
        async def forbidden_respawn(index):
            pytest.fail("Cancelled recovery must not respawn or synthesize")
        monkeypatch.setattr(provider, "_spawn", forbidden_respawn)
        async def produce():
            try:
                return await provider.synthesize("Đã nhận năm trăm nghìn đồng.")
            except asyncio.CancelledError:
                producer_cancelled.set()
                raise
        task = asyncio.create_task(controller.run_singleflight("cancel-recovery", produce, 8))
        await reap_started.wait()
        task.cancel()
        # Producer cancellation must be observed before releasing the reaper.
        while controller._producers.get("cancel-recovery") is not None:
            await asyncio.sleep(0)
        assert controller.active_count == 1 and not task.done()
        release_reap.set()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert producer_cancelled.is_set()
        assert process.returncode is not None and provider._workers[0] is None
        assert provider._idle.qsize() == 2
        assert controller.active_count == controller.total_waiters == 0
    finally:
        release_reap.set()
        if task is not None and not task.done():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        await provider.close()


def test_encoder_emits_short_sentence_before_generating_next(monkeypatch):
    """Framing-only double: this is not real audio or inference timing evidence."""
    import sys
    from types import SimpleNamespace
    from app.providers.piper_worker import stream_encode
    encoded_sentence = b"encoded-sentence"
    flush_calls = []
    class Encoder:
        def __getattr__(self, name):
            return lambda *args: None
        def encode(self, pcm):
            return encoded_sentence
        def flush(self):
            flush_calls.append(True)
            return b"trailer"
    monkeypatch.setitem(sys.modules, "lameenc", SimpleNamespace(Encoder=Encoder))
    monkeypatch.setitem(sys.modules, "piper", SimpleNamespace(SynthesisConfig=lambda **kwargs: kwargs))
    second_generated = []
    def sentences(text, config):
        yield SimpleNamespace(audio_int16_bytes=b"\x00\x00")
        second_generated.append(True)
        yield SimpleNamespace(audio_int16_bytes=b"\x00\x00")
    voice = SimpleNamespace(config=SimpleNamespace(sample_rate=22050), synthesize=sentences)
    encoded = stream_encode(voice, "two sentences", 1.25)
    assert next(encoded) == encoded_sentence
    assert not second_generated and not flush_calls
    assert list(encoded) == [encoded_sentence, b"trailer"]
    assert second_generated and flush_calls == [True]


@pytest.mark.asyncio
@pytest.mark.parametrize("ending", ["missing-end", "error", "empty-end"])
async def test_private_stream_requires_data_and_valid_end(monkeypatch, ending):
    import struct
    from types import SimpleNamespace
    provider = PiperTTSProvider(worker_count=1)
    stdout = asyncio.StreamReader()
    if ending != "empty-end":
        stdout.feed_data(struct.pack("!BI", 1, 5) + b"audio")
    if ending == "error":
        stdout.feed_data(struct.pack("!BI", 2, 0))
    elif ending == "empty-end":
        stdout.feed_data(struct.pack("!BI", 3, 0))
    stdout.feed_eof()
    async def drain():
        pass
    worker = SimpleNamespace(returncode=None, stdout=stdout, stdin=SimpleNamespace(write=lambda _: None, drain=drain))
    provider._workers[0] = worker
    provider._closed = False
    provider._idle.put_nowait(0)
    stopped = []
    async def stop(index, propagate_cancel=True):
        stopped.append(index)
        provider._workers[index] = None
    monkeypatch.setattr(provider, "_stop", stop)
    with pytest.raises((RuntimeError, asyncio.IncompleteReadError)):
        async for _ in provider.stream("protocol fixture"):
            pass
    assert stopped == [0] and provider.idle_count == 1


@pytest.mark.asyncio
async def test_worker_recovery_uses_remaining_total_deadline(monkeypatch):
    provider = PiperTTSProvider(worker_count=1)
    provider._closed = False
    provider._idle.put_nowait(0)
    spawning_closed = asyncio.Event()
    async def spawn(index):
        try:
            await asyncio.Event().wait()
        finally:
            spawning_closed.set()
    monkeypatch.setattr(provider, "_spawn", spawn)
    with pytest.raises(TimeoutError):
        await anext(provider.stream("recovery fixture", timeout_seconds=0.01))
    assert spawning_closed.is_set() and provider.idle_count == 1
