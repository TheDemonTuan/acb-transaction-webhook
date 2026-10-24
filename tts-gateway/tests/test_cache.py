import hashlib
import asyncio
import pytest
from app.cache import TTSCache
from app.schemas import VIENEU_VOICE


def test_cache_key_has_fixed_vieneu_namespace():
    text = "Đã nhận một trăm linh năm nghìn đồng."
    canonical = f"VIENEU_ONLY|{text}|{VIENEU_VOICE}|+0%|+0Hz"
    assert TTSCache.make_key(text, VIENEU_VOICE, "+0%", "+0Hz") == hashlib.sha256(canonical.encode("utf-8")).hexdigest()

@pytest.mark.asyncio
async def test_cache_set_and_get():
    cache = TTSCache(max_items=10, ttl_seconds=2)
    key = cache.make_key("xin chao", VIENEU_VOICE, "+0%", "+0Hz")

    assert await cache.get(key) is None

    await cache.set(key, b"audio_mp3_data", "vieneu", VIENEU_VOICE)
    res = await cache.get(key)
    assert res is not None
    data, provider, voice = res
    assert data == b"audio_mp3_data"
    assert provider == "vieneu"
    assert voice == VIENEU_VOICE

@pytest.mark.asyncio
async def test_cache_single_flight():
    cache = TTSCache(max_items=10, ttl_seconds=10)
    key = cache.make_key("test single flight", VIENEU_VOICE, "+0%", "+0Hz")

    compute_count = 0

    async def _compute():
        nonlocal compute_count
        compute_count += 1
        await asyncio.sleep(0.05)
        return b"computed_mp3", "vieneu", VIENEU_VOICE

    # Launch 5 concurrent requests for the same key
    results = await asyncio.gather(
        cache.get_or_compute(key, _compute),
        cache.get_or_compute(key, _compute),
        cache.get_or_compute(key, _compute),
        cache.get_or_compute(key, _compute),
        cache.get_or_compute(key, _compute),
    )

    # Compute should only be executed ONCE
    assert compute_count == 1
    for data, provider, voice in results:
        assert data == b"computed_mp3"
        assert provider == "vieneu"
        assert voice == VIENEU_VOICE

@pytest.mark.asyncio
async def test_cache_max_bytes_eviction():
    # Cache with max 100 bytes
    cache = TTSCache(max_items=10, max_bytes=100, ttl_seconds=60)
    key1 = cache.make_key("t1", VIENEU_VOICE, "+0%", "+0Hz")
    key2 = cache.make_key("t2", VIENEU_VOICE, "+0%", "+0Hz")

    await cache.set(key1, b"x" * 60, "vieneu", VIENEU_VOICE)
    assert await cache.get(key1) is not None

    # Adding 60 more bytes exceeds 100 bytes, should evict key1
    await cache.set(key2, b"y" * 60, "vieneu", VIENEU_VOICE)
    assert await cache.get(key1) is None
    assert await cache.get(key2) is not None

