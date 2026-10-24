"""Tests for synthesis admission control, cancellation and lifecycle."""
import asyncio
import pytest
from httpx import AsyncClient, ASGITransport
from app.main import app, admission, cache, vieneu_provider

@pytest.fixture(autouse=True)
async def reset_state(monkeypatch):
    monkeypatch.setattr(vieneu_provider, "_closed", False)
    admission.active_count = 0
    admission.waiting_count = 0
    admission.total_waiters = 0
    admission._producers.clear()
    async with cache._lock:
        cache._cache.clear()
        cache._current_bytes = 0

@pytest.mark.asyncio
async def test_admission_overload_cap_matches_configured_workers(monkeypatch):
    """Configured active slots plus sixteen waiting; the next request is rejected."""
    block_event = asyncio.Event()
    capacity = admission.max_active + admission.max_waiting

    async def mock_stream(text, voice="vi-VN-vieneu-v3-turbo-mai-anh-v1", rate="+0%", pitch="+0Hz", timeout_seconds=8.0):
        await block_event.wait()
        yield b"chunk"

    monkeypatch.setattr(vieneu_provider, "stream", mock_stream)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        tasks = []
        for i in range(capacity):
            # Distinct texts avoid singleflight deduplication.
            t = asyncio.create_task(
                client.post("/synthesize/stream", json={"voice": "vi-VN-vieneu-v3-turbo-mai-anh-v1", "provider_mode": "VIENEU_ONLY", **{"text": f"distinct phrase {i}", "cacheable": False}})
            )
            tasks.append(t)

        # Give tasks time to register at the configured cap.
        await asyncio.sleep(0.05)

        assert admission.active_count == admission.max_active
        assert admission.waiting_count == admission.max_waiting

        # One more distinct request must immediately get 503 TTS_OVERLOADED.
        resp19 = await client.post("/synthesize/stream", json={"voice": "vi-VN-vieneu-v3-turbo-mai-anh-v1", "provider_mode": "VIENEU_ONLY", "text": "distinct overflow phrase", "cacheable": False})
        assert resp19.status_code == 503
        assert resp19.headers.get("Retry-After") == "1"
        assert "TTS_OVERLOADED" in resp19.json().get("detail", "")

        # Unblock and cleanup
        block_event.set()
        await asyncio.gather(*tasks, return_exceptions=True)

@pytest.mark.asyncio
async def test_singleflight_waiter_cancellation_does_not_abort_other_waiters(monkeypatch):
    """When waiter A cancels, waiter B still receives completed audio."""
    synth_event = asyncio.Event()

    async def mock_synth(text, voice="vi-VN-vieneu-v3-turbo-mai-anh-v1", rate="+0%", pitch="+0Hz", timeout_seconds=4.0):
        await synth_event.wait()
        return b"mocked_audio_bytes"

    monkeypatch.setattr(vieneu_provider, "synthesize", mock_synth)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        # Launch waiter A and waiter B for the SAME text
        task_a = asyncio.create_task(
            client.post("/synthesize", json={"voice": "vi-VN-vieneu-v3-turbo-mai-anh-v1", "provider_mode": "VIENEU_ONLY", **{"text": "shared phrase", "cacheable": True}})
        )
        task_b = asyncio.create_task(
            client.post("/synthesize", json={"voice": "vi-VN-vieneu-v3-turbo-mai-anh-v1", "provider_mode": "VIENEU_ONLY", **{"text": "shared phrase", "cacheable": True}})
        )

        await asyncio.sleep(0.02)

        # Cancel task A
        task_a.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task_a

        # Release synthesis
        synth_event.set()

        # Task B completes successfully!
        resp_b = await task_b
        assert resp_b.status_code == 200
        assert resp_b.content == b"mocked_audio_bytes"

@pytest.mark.asyncio
async def test_synthesis_timeout_returns_504(monkeypatch):
    """Slow producer exceeding timeout budget returns 504 TTS_TIMEOUT."""
    async def mock_synth(text, voice="vi-VN-vieneu-v3-turbo-mai-anh-v1", rate="+0%", pitch="+0Hz", timeout_seconds=4.0):
        await asyncio.sleep(0.2)
        return b"late_audio"

    monkeypatch.setattr(vieneu_provider, "synthesize", mock_synth)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post("/synthesize", json={"voice": "vi-VN-vieneu-v3-turbo-mai-anh-v1", "provider_mode": "VIENEU_ONLY", **{"text": "timeout test", "timeout_ms": 50, "cacheable": False}})
        assert resp.status_code == 504
        assert "TTS_TIMEOUT" in resp.json().get("detail", "")

@pytest.mark.asyncio
async def test_disconnect_before_first_byte_releases_inference_and_admission(monkeypatch):
    import app.main as main
    from app.schemas import SynthesizeRequest
    entered = asyncio.Event()
    disconnected = asyncio.Event()
    cleaned = asyncio.Event()

    class DisconnectedRequest:
        async def is_disconnected(self):
            return disconnected.is_set()

    async def blocked_stream(**kwargs):
        try:
            entered.set()
            await asyncio.Event().wait()
            yield b"unreachable"
        finally:
            cleaned.set()

    provider = main.vieneu_provider
    monkeypatch.setattr(provider, "stream", blocked_stream)
    monkeypatch.setattr(main, "verify_token", lambda token: None)
    req = SynthesizeRequest(text="Đã nhận năm trăm nghìn đồng.", cacheable=False)
    task = asyncio.create_task(main.synthesize_stream(req, request=DisconnectedRequest()))
    try:
        await asyncio.wait_for(entered.wait(), 1)
        assert admission.active_count == 1
        disconnected.set()
        with pytest.raises(asyncio.CancelledError):
            await asyncio.wait_for(task, 1)
        assert cleaned.is_set()
        assert admission.active_count == admission.waiting_count == admission.total_waiters == 0
    finally:
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
