import pytest
from httpx import AsyncClient, ASGITransport
from app.main import app, edge_provider, edge_circuit

@pytest.mark.asyncio
async def test_health_endpoint():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/health")
        assert resp.status_code == 200
        data = resp.json()
        assert data["status"] == "ok"
        assert data["primary"] == "edge"
        assert "edge" in data

@pytest.mark.asyncio
async def test_voices_endpoint():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/voices")
        assert resp.status_code == 200
        voices = resp.json()
        assert len(voices) == 2
        ids = [v["id"] for v in voices]
        assert "vi-VN-HoaiMyNeural" in ids
        assert "vi-VN-NamMinhNeural" in ids

@pytest.mark.asyncio
async def test_synthesize_edge_success(monkeypatch):
    async def mock_edge_synth(*args, **kwargs):
        return b"fake_edge_audio"

    monkeypatch.setattr(edge_provider, "synthesize", mock_edge_synth)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post(
            "/synthesize",
            json={"text": "Xin chao", "voice": "vi-VN-HoaiMyNeural"},
        )
        assert resp.status_code == 200
        assert resp.headers["X-TTS-Provider"] == "edge"
        assert resp.headers["X-TTS-Voice"] == "vi-VN-HoaiMyNeural"
        assert resp.content == b"fake_edge_audio"

@pytest.mark.asyncio
async def test_synthesize_edge_failure_returns_502(monkeypatch):
    async def mock_edge_fail(*args, **kwargs):
        raise ValueError("Simulated Edge failure")

    monkeypatch.setattr(edge_provider, "synthesize", mock_edge_fail)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post(
            "/synthesize",
            json={"text": "Xin chao", "voice": "vi-VN-HoaiMyNeural", "cacheable": False},
        )
        assert resp.status_code == 502
        assert "TTS_UNAVAILABLE" in resp.json()["detail"]

@pytest.mark.asyncio
async def test_synthesize_stream_edge_success(monkeypatch):
    async def mock_edge_stream(*args, **kwargs):
        yield b"chunk_1_"
        yield b"chunk_2"

    monkeypatch.setattr(edge_provider, "stream", mock_edge_stream)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post(
            "/synthesize/stream",
            json={"text": "Xin chao stream", "voice": "vi-VN-HoaiMyNeural"},
        )
        assert resp.status_code == 200
        assert resp.headers["X-TTS-Provider"] == "edge"
        assert resp.headers["X-TTS-Voice"] == "vi-VN-HoaiMyNeural"
        assert resp.content == b"chunk_1_chunk_2"

@pytest.mark.asyncio
async def test_synthesize_stream_edge_failure_returns_502(monkeypatch):
    async def mock_edge_stream_fail(*args, **kwargs):
        raise ValueError("Edge stream failed")
        yield b""

    monkeypatch.setattr(edge_provider, "stream", mock_edge_stream_fail)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post(
            "/synthesize/stream",
            json={"text": "Xin chao stream fail", "voice": "vi-VN-HoaiMyNeural", "cacheable": False},
        )
        assert resp.status_code == 502
        assert "TTS_UNAVAILABLE" in resp.json()["detail"]

@pytest.mark.asyncio
async def test_cache_max_bytes_configured():
    from app.main import cache
    from app.config import config
    assert cache.max_bytes == config.cache_max_bytes

@pytest.mark.asyncio
async def test_auth_enforcement(monkeypatch):
    from app.config import config
    monkeypatch.setattr(config, "require_auth", True)
    monkeypatch.setattr(config, "internal_token", "secret123")

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        # Request without header fails with 401
        resp = await client.get("/voices")
        assert resp.status_code == 401

        # Request with wrong token fails with 401
        resp = await client.get("/voices", headers={"X-Internal-TTS-Token": "wrong"})
        assert resp.status_code == 401

        # Request with correct token succeeds with 200
        resp = await client.get("/voices", headers={"X-Internal-TTS-Token": "secret123"})
        assert resp.status_code == 200

@pytest.mark.asyncio
async def test_lifespan_fail_when_auth_required_without_token(monkeypatch):
    from app.main import lifespan
    from app.config import config
    monkeypatch.setattr(config, "require_auth", True)
    monkeypatch.setattr(config, "internal_token", "")
    monkeypatch.setattr(config, "token_file", "")

    with pytest.raises(RuntimeError):
        async with lifespan(app):
            pass
