"""FastAPI Application for TTS Gateway."""
import asyncio
from contextlib import asynccontextmanager
import logging
import secrets
from typing import List, Optional
from fastapi import FastAPI, Header, HTTPException, Response, status
from fastapi.responses import JSONResponse, StreamingResponse

from app.config import config
from app.schemas import (
    SynthesizeRequest,
    VoiceItem,
    HealthResponse,
    ProviderHealth,
    ErrorResponse,
)
from app.circuit import CircuitBreaker, CircuitState
from app.cache import TTSCache
from app.providers.edge import EdgeTTSProvider, SUPPORTED_VOICES
from app.providers.gtts import GTTSProvider

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("tts-gateway")

@asynccontextmanager
async def lifespan(app: FastAPI):
    if config.require_auth:
        token = config.get_token()
        if not token:
            raise RuntimeError(
                "TTS_REQUIRE_AUTH is enabled but internal token is missing, unreadable, or empty"
            )
    yield

app = FastAPI(title="TTS Gateway", version="1.0.0", lifespan=lifespan)

edge_provider = EdgeTTSProvider()
gtts_provider = GTTSProvider()
edge_circuit = CircuitBreaker(
    failure_threshold=config.edge_circuit_failure_threshold,
    reset_timeout=config.edge_circuit_reset_timeout,
)
cache = TTSCache(
    max_items=config.cache_max_items,
    max_bytes=config.cache_max_bytes,
    ttl_seconds=config.cache_ttl_seconds,
)

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

@app.get("/health", response_model=HealthResponse)
async def health():
    return HealthResponse(
        status="ok",
        primary="edge",
        edge=ProviderHealth(available=edge_circuit.can_attempt()),
        gtts=ProviderHealth(available=True),
    )

@app.get("/voices", response_model=List[VoiceItem])
async def list_voices(x_internal_tts_token: Optional[str] = Header(None)):
    verify_token(x_internal_tts_token)
    return [
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

@app.post("/synthesize/stream")
async def synthesize_stream(
    req: SynthesizeRequest,
    x_internal_tts_token: Optional[str] = Header(None),
):
    verify_token(x_internal_tts_token)

    text = req.text.strip()
    if not text:
        raise HTTPException(status_code=400, detail="Text cannot be empty")
    if len(text) > config.max_text_length:
        raise HTTPException(
            status_code=400,
            detail=f"Text exceeds maximum length of {config.max_text_length}",
        )

    voice = req.voice or "vi-VN-HoaiMyNeural"
    rate = req.rate or "+0%"
    pitch = req.pitch or "+0Hz"
    is_cacheable = bool(req.cacheable)
    provider_mode = req.provider_mode or "ONLINE_AUTO"
    allow_fallback = bool(req.allow_fallback) and provider_mode == "ONLINE_AUTO"

    cache_key = cache.make_key(text, voice, rate, pitch, allow_fallback, provider_mode)

    if is_cacheable:
        cached = await cache.get(cache_key)
        if cached:
            audio_bytes, provider, actual_voice = cached
            async def _cached_stream():
                yield audio_bytes
            return StreamingResponse(
                _cached_stream(),
                media_type="audio/mpeg",
                headers={
                    "Content-Type": "audio/mpeg",
                    "X-TTS-Provider": provider,
                    "X-TTS-Voice": actual_voice,
                    "X-TTS-Fallback": "true" if provider != "edge" else "false",
                    "X-TTS-Cached": "true",
                },
            )

    if not edge_circuit.can_attempt():
        if not allow_fallback or provider_mode == "EDGE_ONLY":
            raise HTTPException(
                status_code=status.HTTP_502_BAD_GATEWAY,
                detail="TTS_EDGE_CIRCUIT_OPEN: Edge TTS circuit open and fallback is disabled",
            )
        try:
            audio = await gtts_provider.synthesize(text=text, voice="vi", rate=rate, pitch=pitch)
            if is_cacheable:
                await cache.set(cache_key, audio, "gtts", "vi")
            async def _gtts_stream():
                yield audio
            return StreamingResponse(
                _gtts_stream(),
                media_type="audio/mpeg",
                headers={
                    "Content-Type": "audio/mpeg",
                    "X-TTS-Provider": "gtts",
                    "X-TTS-Voice": "vi",
                    "X-TTS-Fallback": "true",
                    "X-TTS-Cached": "false",
                },
            )
        except Exception as e:
            raise HTTPException(
                status_code=status.HTTP_502_BAD_GATEWAY,
                detail=f"TTS_UNAVAILABLE: gTTS fallback failed: {e}",
            )

    try:
        edge_gen = edge_provider.stream(text=text, voice=voice, rate=rate, pitch=pitch)
        first_chunk = await anext(edge_gen)
        edge_circuit.record_success()
    except Exception as e:
        logger.warning(f"Edge TTS stream initialization failed: {e}")
        edge_circuit.record_failure()
        if not allow_fallback or provider_mode == "EDGE_ONLY":
            raise HTTPException(
                status_code=status.HTTP_502_BAD_GATEWAY,
                detail=f"TTS_EDGE_ONLY_FAILED: Edge TTS failed ({e}) and fallback is disabled",
            )
        try:
            audio = await gtts_provider.synthesize(text=text, voice="vi", rate=rate, pitch=pitch)
            if is_cacheable:
                await cache.set(cache_key, audio, "gtts", "vi")
            async def _gtts_fallback_stream():
                yield audio
            return StreamingResponse(
                _gtts_fallback_stream(),
                media_type="audio/mpeg",
                headers={
                    "Content-Type": "audio/mpeg",
                    "X-TTS-Provider": "gtts",
                    "X-TTS-Voice": "vi",
                    "X-TTS-Fallback": "true",
                    "X-TTS-Cached": "false",
                },
            )
        except Exception as fallback_err:
            raise HTTPException(
                status_code=status.HTTP_502_BAD_GATEWAY,
                detail=f"TTS_UNAVAILABLE: both Edge TTS and gTTS failed: {fallback_err}",
            )

    async def _stream_and_tee():
        import io
        collected = io.BytesIO() if is_cacheable else None
        if collected:
            collected.write(first_chunk)
        yield first_chunk
        try:
            async for chunk in edge_gen:
                if collected:
                    collected.write(chunk)
                yield chunk
            if collected:
                await cache.set(cache_key, collected.getvalue(), "edge", voice)
        except Exception as err:
            logger.error(f"Error during Edge stream chunk iteration: {err}")
            edge_circuit.record_failure()

    return StreamingResponse(
        _stream_and_tee(),
        media_type="audio/mpeg",
        headers={
            "Content-Type": "audio/mpeg",
            "X-TTS-Provider": "edge",
            "X-TTS-Voice": voice,
            "X-TTS-Fallback": "false",
            "X-TTS-Cached": "false",
        },
    )

@app.post("/synthesize")
async def synthesize(
    req: SynthesizeRequest,
    x_internal_tts_token: Optional[str] = Header(None),
):
    verify_token(x_internal_tts_token)

    text = req.text.strip()
    if not text:
        raise HTTPException(status_code=400, detail="Text cannot be empty")
    if len(text) > config.max_text_length:
        raise HTTPException(
            status_code=400,
            detail=f"Text exceeds maximum length of {config.max_text_length}",
        )

    voice = req.voice or "vi-VN-HoaiMyNeural"
    rate = req.rate or "+0%"
    pitch = req.pitch or "+0Hz"
    is_cacheable = bool(req.cacheable)
    provider_mode = req.provider_mode or "ONLINE_AUTO"
    allow_fallback = bool(req.allow_fallback) and provider_mode == "ONLINE_AUTO"

    cache_key = cache.make_key(text, voice, rate, pitch, allow_fallback, provider_mode)

    if is_cacheable:
        cached = await cache.get(cache_key)
        if cached:
            audio_bytes, provider, actual_voice = cached
            return Response(
                content=audio_bytes,
                media_type="audio/mpeg",
                headers={
                    "Content-Type": "audio/mpeg",
                    "X-TTS-Provider": provider,
                    "X-TTS-Voice": actual_voice,
                    "X-TTS-Fallback": "true" if provider != "edge" else "false",
                    "X-TTS-Cached": "true",
                },
            )

    async def _execute_synthesis():
        # 1. Try Edge TTS if circuit permits
        if edge_circuit.can_attempt():
            try:
                audio = await edge_provider.synthesize(
                    text=text, voice=voice, rate=rate, pitch=pitch, timeout_seconds=4.0
                )
                edge_circuit.record_success()
                return audio, "edge", voice
            except Exception as e:
                logger.warning(f"Edge TTS synthesis failed, recording circuit failure: {e}")
                edge_circuit.record_failure()
                if not allow_fallback or provider_mode == "EDGE_ONLY":
                    raise HTTPException(
                        status_code=status.HTTP_502_BAD_GATEWAY,
                        detail="TTS_EDGE_ONLY_FAILED: Edge TTS failed and fallback is disabled",
                    )
        else:
            logger.info("Edge circuit is OPEN")
            if not allow_fallback or provider_mode == "EDGE_ONLY":
                raise HTTPException(
                    status_code=status.HTTP_502_BAD_GATEWAY,
                    detail="TTS_EDGE_CIRCUIT_OPEN: Edge TTS circuit open and fallback is disabled",
                )

        # 2. Fallback to gTTS if permitted
        try:
            logger.info("Synthesizing using gTTS fallback")
            audio = await gtts_provider.synthesize(
                text=text, voice="vi", rate=rate, pitch=pitch, timeout_seconds=3.0
            )
            return audio, "gtts", "vi"
        except Exception as e:
            logger.error(f"gTTS fallback synthesis failed: {e}")
            raise HTTPException(
                status_code=status.HTTP_502_BAD_GATEWAY,
                detail="TTS_UNAVAILABLE: both Edge TTS and gTTS providers failed",
            )

    try:
        if is_cacheable:
            audio_bytes, provider, actual_voice = await cache.get_or_compute(
                cache_key, _execute_synthesis
            )
        else:
            audio_bytes, provider, actual_voice = await _execute_synthesis()
    except HTTPException:
        raise
    except Exception as e:
        logger.error(f"Synthesis unexpected failure: {e}")
        raise HTTPException(
            status_code=status.HTTP_502_BAD_GATEWAY,
            detail=f"TTS_SYNTHESIS_FAILED: {str(e)}",
        )

    return Response(
        content=audio_bytes,
        media_type="audio/mpeg",
        headers={
            "Content-Type": "audio/mpeg",
            "X-TTS-Provider": provider,
            "X-TTS-Voice": actual_voice,
            "X-TTS-Fallback": "true" if provider != "edge" else "false",
            "X-TTS-Cached": "false",
        },
    )
