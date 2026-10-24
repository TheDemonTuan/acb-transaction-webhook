"""Regression tests for VieNeu v3 Turbo local provider and gateway integration."""
import pytest
from httpx import AsyncClient, ASGITransport

import app.main as gateway
from app.config import config
from app.providers.vieneu import VieneuTTSProvider, VOICE_ID


def test_provider_defaults_to_one_local_offline_worker(tmp_path):
    provider = VieneuTTSProvider(model_root=tmp_path)
    pool = provider._process_pool
    assert provider.threads == 2 and provider.worker_count == pool.worker_count == 1
    assert pool.worker_module == "app.providers.vieneu_worker"
    assert pool.worker_args == (str(tmp_path.resolve()), "2")
    assert pool.worker_env["HF_HOME"] == str(tmp_path.resolve() / "hf")
    assert pool.worker_env["HF_HUB_CACHE"] == str(tmp_path.resolve() / "hf" / "hub")
    assert pool.worker_env["HF_HUB_OFFLINE"] == "1"
    assert pool.worker_env["HF_HUB_DISABLE_TELEMETRY"] == "1"
    for variable in ("OMP_NUM_THREADS", "OPENBLAS_NUM_THREADS", "MKL_NUM_THREADS"):
        assert pool.worker_env[variable] == "2"


@pytest.mark.parametrize("threads", [1, 2, 3, 4])
def test_provider_accepts_supported_threads(threads):
    assert VieneuTTSProvider(threads=threads).threads == threads


@pytest.mark.parametrize("threads", [0, 5, -1])
def test_provider_rejects_unsupported_threads(threads):
    with pytest.raises(ValueError, match="threads must be between 1 and 4"):
        VieneuTTSProvider(threads=threads)


@pytest.mark.parametrize("workers", [0, 2, 4])
def test_provider_rejects_additional_worker_processes(workers):
    with pytest.raises(ValueError, match="only supports 1 worker process"):
        VieneuTTSProvider(worker_count=workers)


@pytest.mark.asyncio
async def test_vieneu_worker_synthesize_and_stream():
    provider = VieneuTTSProvider(threads=2)
    try:
        await provider.start()
        assert provider.available and provider.worker_count == 1
        processes = tuple(provider._workers)
        text = "Đã nhận một triệu không trăm linh năm nghìn đồng."
        mp3_bytes = await provider.synthesize(text)
        assert len(mp3_bytes) > 1000
        # Validate MP3 sync frame (first 2 bytes) or ID3
        assert mp3_bytes[:3] == b"ID3" or (mp3_bytes[0] == 0xFF and (mp3_bytes[1] & 0xE0) == 0xE0)

        # Test streaming
        chunks = []
        async for chunk in provider.stream(text):
            chunks.append(chunk)
        streamed_bytes = b"".join(chunks)
        assert len(streamed_bytes) <= 4 * 1024 * 1024
        assert streamed_bytes[:3] == b"ID3" or (streamed_bytes[0] == 0xFF and (streamed_bytes[1] & 0xE0) == 0xE0)
        assert tuple(provider._workers) == processes
    finally:
        await provider.close()
    assert all(process.returncode is not None for process in processes)
    assert not provider.available


@pytest.mark.asyncio
async def test_vieneu_invalid_rate_or_pitch_fails():
    provider = VieneuTTSProvider(threads=2)
    with pytest.raises(ValueError, match="Unsupported VieNeu rate"):
        await provider.synthesize("Đã nhận tiền.", rate="+10%")
    with pytest.raises(ValueError, match="Unsupported VieNeu pitch"):
        await provider.synthesize("Đã nhận tiền.", pitch="+5Hz")
    with pytest.raises(ValueError, match="Unsupported VieNeu voice"):
        await provider.synthesize("Đã nhận tiền.", voice="invalid-voice")


@pytest.mark.asyncio
async def test_vieneu_http_endpoints_and_headers(monkeypatch):
    monkeypatch.setattr(config, "require_auth", False)
    monkeypatch.setattr(config, "internal_token", "")
    monkeypatch.setattr(config, "token_file", "")

    async with gateway.lifespan(gateway.app):
        assert gateway.vieneu_provider.available
        async with AsyncClient(transport=ASGITransport(app=gateway.app), base_url="http://test") as client:
            health = (await client.get("/health")).json()
            assert health["primary"] == "vieneu"
            assert health["vieneu"]["available"] is True
            assert set(health) == {"status", "primary", "vieneu"}
            assert health["vieneu"]["workerCount"] == 1

            # POST /synthesize with VIENEU_ONLY
            payload = {
                "text": "Đã nhận một trăm linh năm nghìn đồng.",
                "voice": VOICE_ID,
                "provider_mode": "VIENEU_ONLY",
                "rate": "+0%",
                "pitch": "+0Hz",
                "cacheable": False,
            }
            resp = await client.post("/synthesize", json=payload)
            assert resp.status_code == 200
            assert resp.headers["content-type"] == "audio/mpeg"
            assert resp.headers["x-tts-provider"] == "vieneu"
            assert resp.headers["x-tts-voice"] == VOICE_ID
            assert resp.headers["x-tts-fallback"] == "false"
            assert len(resp.content) > 1000

            # POST /synthesize/stream with VIENEU_ONLY
            resp_stream = await client.post("/synthesize/stream", json=payload)
            assert resp_stream.status_code == 200
            assert resp_stream.headers["content-type"] == "audio/mpeg"
            assert resp_stream.headers["x-tts-voice"] == VOICE_ID
            assert resp_stream.headers["x-tts-provider"] == "vieneu"
            assert resp_stream.headers["x-tts-fallback"] == "false"
            assert len(resp_stream.content) > 1000
