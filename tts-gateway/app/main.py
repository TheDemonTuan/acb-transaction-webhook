"""Fixed-provider TTS with bounded admission and immutable local Piper models."""
import asyncio
from contextlib import asynccontextmanager
import logging
import os
import secrets
import anyio
from typing import List, Optional
from fastapi import FastAPI, Header, HTTPException, Request, Response, status
from fastapi.responses import StreamingResponse

from app.config import config
from app.schemas import (
    SynthesizeRequest,
    VoiceItem,
    HealthResponse,
    ProviderHealth,
)
from app.circuit import CircuitBreaker
from app.cache import TTSCache
from app.admission import AdmissionController, remaining, timeout_error
from app.assets import AssetStore
from app.providers.edge import EdgeTTSProvider
from app.providers.piper import PiperTTSProvider, VOICE_ID

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("tts-gateway")

MAX_AUDIO_BYTES = 4 * 1024 * 1024  # 4 MiB max per audio response

@asynccontextmanager
async def lifespan(app: FastAPI):
    if config.require_auth:
        token = config.get_token()
        if not token:
            raise RuntimeError(
                "TTS_REQUIRE_AUTH is enabled but internal token is missing, unreadable, or empty"
            )
    try:
        await piper_provider.start()
    except Exception:
        logger.exception("Fixed Piper provider unavailable; no provider fallback")
    try:
        yield
    finally:
        with anyio.CancelScope(shield=True):
            await piper_provider.close()

app = FastAPI(title="TTS Gateway", version="2.0.0", lifespan=lifespan)

edge_provider = EdgeTTSProvider()
piper_provider = PiperTTSProvider()
edge_circuit = CircuitBreaker(
    failure_threshold=config.edge_circuit_failure_threshold,
    reset_timeout=config.edge_circuit_reset_timeout,
)
cache = TTSCache(
    max_items=config.cache_max_items,
    max_bytes=config.cache_max_bytes,
    ttl_seconds=config.cache_ttl_seconds,
)
admission = AdmissionController(
    max_active=2,
    max_waiting=16,
    max_total_waiters=64,
)

assets = AssetStore(os.environ.get("TTS_VOICEPACK_ROOT", "/app/assets/voicepacks"))

def verify_token(x_internal_tts_token: Optional[str]):
    expected = config.get_token()
    if not expected:
        if config.require_auth:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail="Internal TTS token required by configuration but not found",
            )
        return
    if not x_internal_tts_token or not secrets.compare_digest(x_internal_tts_token, expected):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Unauthorized: invalid internal TTS token",
        )

def verify_asset_token(token: Optional[str]):
    # Published audio never inherits the development synthesis auth opt-out.
    expected = config.get_token()
    if not expected:
        raise HTTPException(status_code=503, detail="VOICE_ASSET_AUTH_UNAVAILABLE")
    if not token or not secrets.compare_digest(token, expected):
        raise HTTPException(status_code=401, detail="Unauthorized: invalid internal TTS token")


@app.get("/assets/profiles/{synthesis_key}/manifest")
def voice_manifest(
    synthesis_key: str,
    x_internal_tts_token: Optional[str] = Header(None),
    if_none_match: Optional[str] = Header(None),
):
    verify_asset_token(x_internal_tts_token)
    try:
        pack = assets.manifest(synthesis_key)
    except KeyError:
        raise HTTPException(status_code=404, detail="VOICE_PACK_UNAVAILABLE") from None
    etag = '"' + pack.manifest["packVersion"] + '"'
    headers = {"Cache-Control": "no-cache", "ETag": etag}
    if if_none_match == etag:
        return Response(status_code=304, headers=headers)
    return Response(content=pack.body, media_type="application/json", headers=headers)


@app.get("/assets/clips/{sha256}")
def voice_clip(sha256: str, x_internal_tts_token: Optional[str] = Header(None)):
    verify_asset_token(x_internal_tts_token)
    try:
        audio = assets.clip(sha256)
    except KeyError:
        raise HTTPException(status_code=404, detail="VOICE_PACK_UNAVAILABLE") from None
    return Response(content=audio, media_type="audio/mpeg", headers={
        "Cache-Control": "public,max-age=31536000,immutable",
        "ETag": '"' + sha256 + '"',
        "X-Content-Type-Options": "nosniff",
    })


@app.get("/health", response_model=HealthResponse)
async def health():
    return HealthResponse(
        status="ok" if piper_provider.available else "degraded",
        primary="piper",
        edge=ProviderHealth(available=edge_circuit.can_attempt()),
        piper=ProviderHealth(available=piper_provider.available),
    )

@app.get("/voices", response_model=List[VoiceItem])
async def list_voices(x_internal_tts_token: Optional[str] = Header(None)):
    verify_token(x_internal_tts_token)
    return [
        VoiceItem(id=VOICE_ID, name="VAIS 1000 (Piper tiếng Việt)", gender="unspecified", provider="piper"),
        VoiceItem(
            id="vi-VN-HoaiMyNeural",
            name="Hoài My",
            gender="female",
            provider="edge",
        ),
        VoiceItem(
            id="vi-VN-NamMinhNeural",
            name="Nam Minh",
            gender="male",
            provider="edge",
        ),
    ]

def audio_headers(voice, cached=False):
    return {
        "X-TTS-Provider": "piper" if voice == VOICE_ID else "edge", "X-TTS-Voice": voice,
        "X-TTS-Fallback": "false", "X-TTS-Cached": str(cached).lower(),
    }


def synthesis_parameters(req):
    text = req.text.strip()
    if not text or len(text) > config.max_text_length:
        raise HTTPException(status_code=400, detail="INVALID_TTS_TEXT")
    voice = req.voice
    rate, pitch = req.rate, req.pitch
    key = cache.make_key(text, voice, rate, pitch, False, req.provider_mode)
    return text, voice, rate, pitch, key


class OwnedStreamingResponse(StreamingResponse):
    """Own cleanup even when ASGI disconnects before iterating the first byte."""
    def __init__(self, iterator, cleanup, deadline, **kwargs):
        super().__init__(iterator, **kwargs)
        self.cleanup = cleanup
        self.deadline = deadline

    async def __call__(self, scope, receive, send):
        try:
            # Includes backpressure/downstream sends, not just upstream reads.
            async with asyncio.timeout_at(self.deadline):
                await super().__call__(scope, receive, send)
        finally:
            with anyio.CancelScope(shield=True):
                try:
                    await self.body_iterator.aclose()
                finally:
                    await self.cleanup()


@app.post("/synthesize/stream")
async def synthesize_stream(req: SynthesizeRequest, x_internal_tts_token: Optional[str] = Header(None), request: Request = None):
    verify_token(x_internal_tts_token)
    text, voice, rate, pitch, key = synthesis_parameters(req)
    if req.cacheable:
        cached = await cache.get(key)
        if cached:
            return Response(content=cached[0], media_type="audio/mpeg", headers=audio_headers(cached[2], True))
    if req.provider_mode == "PIPER_ONLY":
        deadline = asyncio.get_running_loop().time() + req.timeout_ms / 1000
        # CPU inference is singleflight and completes before headers; disconnects
        # cannot leave background inference consuming a released admission slot.
        response = await synthesize(req, x_internal_tts_token, request)
        async def chunks():
            for offset in range(0, len(response.body), 16384):
                yield response.body[offset:offset + 16384]
        iterator = chunks()
        return OwnedStreamingResponse(iterator, iterator.aclose, deadline, media_type="audio/mpeg", headers=dict(response.headers))
    if not edge_circuit.can_attempt():
        raise HTTPException(status_code=502, detail="TTS_UNAVAILABLE")

    deadline = asyncio.get_running_loop().time() + req.timeout_ms / 1000
    await admission.register_waiter()
    slot = None
    generator = None
    transferred = False
    cleaned = False

    async def cleanup():
        nonlocal cleaned
        if cleaned:
            return
        cleaned = True
        try:
            if generator is not None:
                await generator.aclose()
        finally:
            try:
                if slot is not None:
                    await slot.__aexit__(None, None, None)
            finally:
                await admission.unregister_waiter()

    try:
        candidate = admission.acquire_slot(remaining(deadline))
        await candidate.__aenter__()
        slot = candidate
        generator = edge_provider.stream(text=text, voice=voice, rate=rate, pitch=pitch)
        first = await asyncio.wait_for(anext(generator), min(config.edge_stream_initial_timeout, remaining(deadline)))
        if not first or len(first) > MAX_AUDIO_BYTES:
            raise HTTPException(status_code=502, detail="TTS_AUDIO_TOO_LARGE")

        async def stream_and_tee():
            collected = bytearray() if req.cacheable else None
            total = 0
            chunk = first
            try:
                while True:
                    remaining(deadline)
                    total += len(chunk)
                    if total > MAX_AUDIO_BYTES:
                        raise RuntimeError("TTS_AUDIO_TOO_LARGE")
                    if collected is not None:
                        collected.extend(chunk)
                    # First yield is inside the same ownership finally as all later yields.
                    yield chunk
                    try:
                        chunk = await asyncio.wait_for(anext(generator), min(config.edge_stream_idle_timeout, remaining(deadline)))
                    except StopAsyncIteration:
                        break
                remaining(deadline)
                edge_circuit.record_success()
                if collected is not None:
                    await cache.set(key, bytes(collected), "edge", voice)
            except Exception:
                edge_circuit.record_failure()
                raise
            finally:
                if collected is not None:
                    collected.clear()
                with anyio.CancelScope(shield=True):
                    await cleanup()

        response = OwnedStreamingResponse(stream_and_tee(), cleanup, deadline, media_type="audio/mpeg", headers=audio_headers(voice))
        transferred = True
        return response
    except TimeoutError:
        raise timeout_error() from None
    except HTTPException:
        raise
    except Exception:
        edge_circuit.record_failure()
        raise HTTPException(status_code=502, detail="TTS_UNAVAILABLE") from None
    finally:
        if not transferred:
            with anyio.CancelScope(shield=True):
                await cleanup()


@app.post("/synthesize")
async def synthesize(req: SynthesizeRequest, x_internal_tts_token: Optional[str] = Header(None), request: Request = None):
    verify_token(x_internal_tts_token)
    text, voice, rate, pitch, key = synthesis_parameters(req)
    if req.cacheable:
        cached = await cache.get(key)
        if cached:
            return Response(content=cached[0], media_type="audio/mpeg", headers=audio_headers(cached[2], True))
    is_piper = req.provider_mode == "PIPER_ONLY"
    if (is_piper and piper_provider._closed) or (not is_piper and not edge_circuit.can_attempt()):
        raise HTTPException(status_code=502, detail="TTS_UNAVAILABLE")
    provider = piper_provider if is_piper else edge_provider
    provider_name = "piper" if is_piper else "edge"

    deadline = asyncio.get_running_loop().time() + req.timeout_ms / 1000

    async def execute():
        try:
            audio = await provider.synthesize(text=text, voice=voice, rate=rate, pitch=pitch, timeout_seconds=remaining(deadline))
            if not audio or len(audio) > MAX_AUDIO_BYTES:
                raise HTTPException(status_code=502, detail="TTS_AUDIO_TOO_LARGE")
            remaining(deadline)
            if not is_piper:
                edge_circuit.record_success()
            if req.cacheable:
                await cache.set(key, audio, provider_name, voice)
            return audio, provider_name, voice
        except TimeoutError:
            raise timeout_error() from None
        except HTTPException:
            raise
        except Exception:
            if not is_piper:
                edge_circuit.record_failure()
            raise HTTPException(status_code=502, detail="TTS_UNAVAILABLE") from None

    audio, _, actual_voice = await admission.run_singleflight(key, execute, remaining(deadline), request.is_disconnected if request is not None else None)
    return Response(content=audio, media_type="audio/mpeg", headers=audio_headers(actual_voice))
