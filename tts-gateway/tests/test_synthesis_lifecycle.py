"""Tests for synthesis admission control, cancellation and lifecycle."""
import asyncio
import pytest
from httpx import AsyncClient, ASGITransport
from app.main import app, admission, cache, edge_provider

@pytest.fixture(autouse=True)
async def reset_state():
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

    async def mock_stream(text, voice="vi-VN-HoaiMyNeural", rate="+0%", pitch="+0Hz"):
        await block_event.wait()
        yield b"chunk"

    monkeypatch.setattr(edge_provider, "stream", mock_stream)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        tasks = []
        for i in range(capacity):
            # Distinct texts avoid singleflight deduplication.
            t = asyncio.create_task(
                client.post("/synthesize/stream", json={"voice": "vi-VN-HoaiMyNeural", "provider_mode": "EDGE_ONLY", **{"text": f"distinct phrase {i}", "cacheable": False}})
            )
            tasks.append(t)

        # Give tasks time to register at the configured cap.
        await asyncio.sleep(0.05)

        assert admission.active_count == admission.max_active
        assert admission.waiting_count == admission.max_waiting

        # One more distinct request must immediately get 503 TTS_OVERLOADED.
        resp19 = await client.post("/synthesize/stream", json={"voice": "vi-VN-HoaiMyNeural", "provider_mode": "EDGE_ONLY", "text": "distinct overflow phrase", "cacheable": False})
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

    async def mock_synth(text, voice="vi-VN-HoaiMyNeural", rate="+0%", pitch="+0Hz", timeout_seconds=4.0):
        await synth_event.wait()
        return b"mocked_audio_bytes"

    monkeypatch.setattr(edge_provider, "synthesize", mock_synth)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        # Launch waiter A and waiter B for the SAME text
        task_a = asyncio.create_task(
            client.post("/synthesize", json={"voice": "vi-VN-HoaiMyNeural", "provider_mode": "EDGE_ONLY", **{"text": "shared phrase", "cacheable": True}})
        )
        task_b = asyncio.create_task(
            client.post("/synthesize", json={"voice": "vi-VN-HoaiMyNeural", "provider_mode": "EDGE_ONLY", **{"text": "shared phrase", "cacheable": True}})
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
    async def mock_synth(text, voice="vi-VN-HoaiMyNeural", rate="+0%", pitch="+0Hz", timeout_seconds=4.0):
        await asyncio.sleep(0.2)
        return b"late_audio"

    monkeypatch.setattr(edge_provider, "synthesize", mock_synth)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post("/synthesize", json={"voice": "vi-VN-HoaiMyNeural", "provider_mode": "EDGE_ONLY", **{"text": "timeout test", "timeout_ms": 50, "cacheable": False}})
        assert resp.status_code == 504
        assert "TTS_TIMEOUT" in resp.json().get("detail", "")
