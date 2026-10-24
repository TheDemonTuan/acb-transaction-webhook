import pytest
from httpx import AsyncClient, ASGITransport
import app.main as gateway
from app.admission import AdmissionController
from app.cache import TTSCache
from app.config import config
from app.main import app, vieneu_provider
from app.providers.vieneu import VOICE_ID


@pytest.fixture(autouse=True)
def isolated(monkeypatch):
    monkeypatch.setattr(gateway, "admission", AdmissionController(max_active=1))
    monkeypatch.setattr(gateway, "cache", TTSCache())
    monkeypatch.setattr(vieneu_provider, "_closed", False)
    monkeypatch.setattr(config, "require_auth", False)
    monkeypatch.setattr(config, "internal_token", "")
    monkeypatch.setattr(config, "token_file", "")

@pytest.mark.asyncio
@pytest.mark.parametrize("available", [False, True])
async def test_health_endpoint(monkeypatch, available):
    monkeypatch.setattr(type(vieneu_provider), "available", property(lambda self: available))
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/health")
        assert resp.status_code == 200
        data = resp.json()
        assert set(data) == {"status", "primary", "vieneu"}
        assert data["status"] == ("ok" if available else "degraded")
        assert data["primary"] == "vieneu"
        assert data["vieneu"] == {
            "available": available,
            "workerCount": 1,
            "activeWorkers": vieneu_provider.active_count,
            "idleWorkers": vieneu_provider.idle_count,
        }

@pytest.mark.asyncio
async def test_voices_endpoint():
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.get("/voices")
        assert resp.status_code == 200
        assert resp.json() == [{
            "id": VOICE_ID,
            "name": "Mai Anh · nữ miền Bắc · VieNeu local",
            "gender": "female",
            "provider": "vieneu",
        }]

@pytest.mark.asyncio
async def test_synthesize_vieneu_success(monkeypatch):
    async def mock_synth(**kwargs):
        assert kwargs["voice"] == VOICE_ID
        assert kwargs["rate"] == "+0%"
        assert kwargs["pitch"] == "+0Hz"
        assert 0 < kwargs["timeout_seconds"] <= 8
        return b"fake_vieneu_audio"

    monkeypatch.setattr(vieneu_provider, "synthesize", mock_synth)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post("/synthesize", json={"text": "Xin chao"})
        assert resp.status_code == 200
        assert resp.headers["X-TTS-Provider"] == "vieneu"
        assert resp.headers["X-TTS-Voice"] == VOICE_ID
        assert resp.headers["X-TTS-Fallback"] == "false"
        assert resp.headers["X-TTS-Cached"] == "false"
        assert resp.content == b"fake_vieneu_audio"
        cached = await client.post("/synthesize", json={"text": "Xin chao"})
        assert cached.content == resp.content
        assert cached.headers["X-TTS-Provider"] == "vieneu"
        assert cached.headers["X-TTS-Voice"] == VOICE_ID
        assert cached.headers["X-TTS-Fallback"] == "false"
        assert cached.headers["X-TTS-Cached"] == "true"

@pytest.mark.asyncio
async def test_synthesize_vieneu_failure_returns_502(monkeypatch):
    async def mock_fail(**kwargs):
        raise ValueError("Simulated VieNeu failure")

    monkeypatch.setattr(vieneu_provider, "synthesize", mock_fail)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post("/synthesize", json={"text": "Xin chao", "cacheable": False})
        assert resp.status_code == 502
        assert resp.json()["detail"] == "TTS_UNAVAILABLE"

@pytest.mark.asyncio
async def test_synthesize_stream_vieneu_success(monkeypatch):
    async def mock_stream(**kwargs):
        assert kwargs["voice"] == VOICE_ID
        assert kwargs["rate"] == "+0%"
        assert kwargs["pitch"] == "+0Hz"
        assert 0 < kwargs["timeout_seconds"] <= 8
        yield b"chunk_1_"
        yield b"chunk_2"

    monkeypatch.setattr(vieneu_provider, "stream", mock_stream)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post("/synthesize/stream", json={"text": "Xin chao stream"})
        assert resp.status_code == 200
        assert resp.headers["X-TTS-Provider"] == "vieneu"
        assert resp.headers["X-TTS-Voice"] == VOICE_ID
        assert resp.headers["X-TTS-Fallback"] == "false"
        assert resp.content == b"chunk_1_chunk_2"
        cached = await client.post("/synthesize/stream", json={"text": "Xin chao stream"})
        assert cached.content == resp.content
        assert cached.headers["X-TTS-Provider"] == "vieneu"
        assert cached.headers["X-TTS-Voice"] == VOICE_ID
        assert cached.headers["X-TTS-Fallback"] == "false"
        assert cached.headers["X-TTS-Cached"] == "true"

@pytest.mark.asyncio
async def test_synthesize_stream_vieneu_failure_returns_502(monkeypatch):
    async def mock_stream_fail(**kwargs):
        raise ValueError("VieNeu stream failed")
        yield b""

    monkeypatch.setattr(vieneu_provider, "stream", mock_stream_fail)

    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post("/synthesize/stream", json={"text": "Xin chao stream fail", "cacheable": False})
        assert resp.status_code == 502
        assert resp.json()["detail"] == "TTS_UNAVAILABLE"

@pytest.mark.asyncio
@pytest.mark.parametrize("endpoint", ["/synthesize", "/synthesize/stream"])
@pytest.mark.parametrize("overrides", [
    {"provider_mode": "EDGE_ONLY"},
    {"provider_mode": "PIPER_ONLY"},
    {"voice": "vi-VN-HoaiMyNeural"},
    {"voice": "vi-VN-NamMinhNeural"},
    {"voice": "vi-VN-vais1000-medium"},
    {"rate": "+25%"},
    {"pitch": "+1Hz"},
])
async def test_synthesis_rejects_non_vieneu_profiles(monkeypatch, endpoint, overrides):
    async def forbidden(**kwargs):
        pytest.fail("Invalid profiles must never invoke inference")

    monkeypatch.setattr(vieneu_provider, "synthesize", forbidden)
    monkeypatch.setattr(vieneu_provider, "stream", forbidden)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        response = await client.post(endpoint, json={"text": "Đã nhận tiền.", **overrides})
    assert response.status_code == 422


@pytest.mark.asyncio
@pytest.mark.parametrize("endpoint", ["/synthesize", "/synthesize/stream"])
async def test_synthesis_requires_authentication(monkeypatch, endpoint):
    monkeypatch.setattr(config, "require_auth", True)
    monkeypatch.setattr(config, "internal_token", "secret123")
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        response = await client.post(endpoint, json={"text": "Đã nhận tiền."})
    assert response.status_code == 401


@pytest.mark.asyncio
@pytest.mark.parametrize("endpoint", ["/synthesize", "/synthesize/stream"])
async def test_closed_provider_is_unavailable(monkeypatch, endpoint):
    monkeypatch.setattr(vieneu_provider, "_closed", True)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        response = await client.post(endpoint, json={"text": "Đã nhận tiền.", "cacheable": False})
    assert response.status_code == 502
    assert response.json()["detail"] == "TTS_UNAVAILABLE"


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


@pytest.mark.asyncio
@pytest.mark.parametrize("startup_fails", [False, True])
async def test_lifespan_starts_and_closes_vieneu(monkeypatch, startup_fails):
    calls = []

    async def start():
        calls.append("start")
        if startup_fails:
            raise RuntimeError("VieNeu unavailable")

    async def close():
        calls.append("close")

    monkeypatch.setattr(vieneu_provider, "start", start)
    monkeypatch.setattr(vieneu_provider, "close", close)
    async with gateway.lifespan(app):
        assert calls == ["start"]
    assert calls == ["start", "close"]
