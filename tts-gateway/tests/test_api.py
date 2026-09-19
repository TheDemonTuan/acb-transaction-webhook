import pytest
from httpx import AsyncClient, ASGITransport
from app.main import app, edge_provider, gtts_provider, edge_circuit

@pytest.mark.asyncio
async def test_health_endpoint():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        resp = await ac.get("/health")
        assert resp.status_code == 200
        data = resp.json()
        assert data["status"] == "ok"
        assert data["primary"] == "edge"
        assert "edge" in data
        assert "gtts" in data

@pytest.mark.asyncio
async def test_voices_endpoint():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        resp = await ac.get("/voices")
        assert resp.status_code == 200
        voices = resp.json()
        assert len(voices) >= 2
        ids = [v["id"] for v in voices]
        assert "vi-VN-HoaiMyNeural" in ids
        assert "vi-VN-NamMinhNeural" in ids

@pytest.mark.asyncio
async def test_synthesize_edge_success(monkeypatch):
    async def mock_edge_synth(*args, **kwargs):
        return b"fake_edge_audio"

    monkeypatch.setattr(edge_provider, "synthesize", mock_edge_synth)
    edge_circuit.record_success()

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        resp = await ac.post(
            "/synthesize",
            json={"text": "Bạn vừa nhận được 500k", "voice": "vi-VN-HoaiMyNeural"},
        )
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "audio/mpeg"
        assert resp.headers["x-tts-provider"] == "edge"
        assert resp.headers["x-tts-fallback"] == "false"
        assert resp.content == b"fake_edge_audio"

@pytest.mark.asyncio
async def test_synthesize_fallback_to_gtts(monkeypatch):
    async def mock_edge_fail(*args, **kwargs):
        raise ConnectionError("Edge connection timed out")

    async def mock_gtts_success(*args, **kwargs):
        return b"fake_gtts_audio"

    monkeypatch.setattr(edge_provider, "synthesize", mock_edge_fail)
    monkeypatch.setattr(gtts_provider, "synthesize", mock_gtts_success)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        resp = await ac.post(
            "/synthesize",
            json={"text": "Bạn vừa nhận được 200k", "voice": "vi-VN-HoaiMyNeural", "cacheable": False},
        )
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "audio/mpeg"
        assert resp.headers["x-tts-provider"] == "gtts"
        assert resp.headers["x-tts-fallback"] == "true"
        assert resp.content == b"fake_gtts_audio"

@pytest.mark.asyncio
async def test_synthesize_stream_edge_success(monkeypatch):
    async def mock_edge_stream(*args, **kwargs):
        yield b"chunk_1_"
        yield b"chunk_2"

    monkeypatch.setattr(edge_provider, "stream", mock_edge_stream)
    edge_circuit.record_success()

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        resp = await ac.post(
            "/synthesize/stream",
            json={"text": "Bạn vừa nhận được 999k stream", "voice": "vi-VN-HoaiMyNeural", "cacheable": True},
        )
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "audio/mpeg"
        assert resp.headers["x-tts-provider"] == "edge"
        assert resp.headers["x-tts-fallback"] == "false"
        assert resp.content == b"chunk_1_chunk_2"

@pytest.mark.asyncio
async def test_synthesize_stream_fallback_to_gtts(monkeypatch):
    async def mock_edge_stream_fail(*args, **kwargs):
        raise ConnectionError("Edge stream connection timed out")
        yield b""

    async def mock_gtts_success(*args, **kwargs):
        return b"gtts_stream_audio"

    monkeypatch.setattr(edge_provider, "stream", mock_edge_stream_fail)
    monkeypatch.setattr(gtts_provider, "synthesize", mock_gtts_success)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        resp = await ac.post(
            "/synthesize/stream",
            json={"text": "Bạn vừa nhận được 888k fallback stream", "voice": "vi-VN-HoaiMyNeural", "cacheable": False},
        )
        assert resp.status_code == 200
        assert resp.headers["content-type"] == "audio/mpeg"
        assert resp.headers["x-tts-provider"] == "gtts"
        assert resp.headers["x-tts-fallback"] == "true"
        assert resp.content == b"gtts_stream_audio"

@pytest.mark.asyncio
async def test_cache_max_bytes_configured():
    from app.main import cache
    from app.config import config
    assert cache.max_bytes == config.cache_max_bytes

@pytest.mark.asyncio
async def test_auth_enforcement(monkeypatch):
    from app.config import config
    monkeypatch.setattr(config, "internal_token", "secret-test-token")
    monkeypatch.setattr(config, "token_file", "")
    monkeypatch.setattr(config, "require_auth", True)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as ac:
        # 1. No token -> 401
        resp = await ac.get("/voices")
        assert resp.status_code == 401

        # 2. Invalid token -> 401
        resp = await ac.get("/voices", headers={"X-Internal-TTS-Token": "wrong-token"})
        assert resp.status_code == 401

        # 3. Valid token -> 200
        resp = await ac.get("/voices", headers={"X-Internal-TTS-Token": "secret-test-token"})
        assert resp.status_code == 200

@pytest.mark.asyncio
async def test_lifespan_fail_when_auth_required_without_token(monkeypatch):
    from app.main import lifespan
    from app.config import config
    monkeypatch.setattr(config, "internal_token", "")
    monkeypatch.setattr(config, "token_file", "")
    monkeypatch.setattr(config, "require_auth", True)

    with pytest.raises(RuntimeError, match="TTS_REQUIRE_AUTH is enabled but internal token is missing"):
        async with lifespan(app):
            pass
