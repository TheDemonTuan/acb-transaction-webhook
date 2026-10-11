"""Deterministic lifecycle fault injection; never contacts Edge."""
import asyncio
from contextlib import AsyncExitStack

import pytest
from fastapi import HTTPException
from httpx import ASGITransport, AsyncClient
from starlette.requests import ClientDisconnect

import app.main as gateway
from app.admission import AdmissionController
from app.cache import TTSCache
from app.circuit import CircuitBreaker
from app.config import config
from app.schemas import SynthesizeRequest


@pytest.fixture(autouse=True)
def isolated(monkeypatch):
    monkeypatch.setattr(gateway, "admission", AdmissionController())
    monkeypatch.setattr(gateway, "cache", TTSCache())
    monkeypatch.setattr(gateway, "edge_circuit", CircuitBreaker())
    monkeypatch.setattr(config, "require_auth", False)
    monkeypatch.setattr(config, "internal_token", "")
    monkeypatch.setattr(config, "token_file", "")


async def until(predicate):
    async with asyncio.timeout(2):
        while not predicate():
            await asyncio.sleep(0)


def assert_empty(controller):
    assert (controller.active_count, controller.waiting_count, controller.total_waiters) == (0, 0, 0)
    assert not controller._producers


@pytest.mark.asyncio
@pytest.mark.parametrize("phase", ["queued", "first-byte", "first-yield", "buffered"])
async def test_64_actual_cancellations_release_all_ownership_and_recover(monkeypatch, phase):
    controller = gateway.admission
    closed = 0
    started = asyncio.Event()
    blocker = asyncio.Event()

    async def stream(**kwargs):
        nonlocal closed
        try:
            started.set()
            if phase != "first-yield":
                await blocker.wait()
            yield b"first"
            await blocker.wait()
        finally:
            closed += 1

    async def buffered(**kwargs):
        started.set()
        await blocker.wait()
        return b"complete"

    monkeypatch.setattr(gateway.edge_provider, "stream", stream)
    monkeypatch.setattr(gateway.edge_provider, "synthesize", buffered)
    async with AsyncExitStack() as stack:
        if phase == "queued":
            await stack.enter_async_context(controller.acquire_slot(8))
            await stack.enter_async_context(controller.acquire_slot(8))
        for index in range(64):
            started.clear()
            request = SynthesizeRequest(text=f"cycle {index}", cacheable=False)
            if phase == "first-yield":
                response = await gateway.synthesize_stream(request)
                assert await anext(response.body_iterator) == b"first"
                await response.body_iterator.aclose()
            else:
                handler = gateway.synthesize if phase == "buffered" else gateway.synthesize_stream
                task = asyncio.create_task(handler(request))
                if phase == "queued":
                    await until(lambda: controller.waiting_count == 1)
                else:
                    await started.wait()
                task.cancel()
                with pytest.raises(asyncio.CancelledError):
                    await task
            assert controller.total_waiters == 0
            assert controller.waiting_count == 0
            assert not controller._producers
        if phase != "queued":
            assert controller.active_count == 0
    assert_empty(controller)
    if phase in ("first-byte", "first-yield"):
        assert closed == 64

    async def recovered(**kwargs):
        return b"recovered"
    monkeypatch.setattr(gateway.edge_provider, "synthesize", recovered)
    result = await gateway.synthesize(SynthesizeRequest(text="recovery", cacheable=False))
    assert result.body == b"recovered"
    assert_empty(controller)


@pytest.mark.asyncio
async def test_response_send_failure_before_first_yield_still_closes_upstream(monkeypatch):
    closed = asyncio.Event()
    async def stream(**kwargs):
        try:
            yield b"first"
            await asyncio.Event().wait()
        finally:
            closed.set()
    monkeypatch.setattr(gateway.edge_provider, "stream", stream)
    response = await gateway.synthesize_stream(SynthesizeRequest(text="header disconnect", cacheable=False))
    async def receive():
        await asyncio.Event().wait()
    async def send(message):
        raise OSError("disconnected before body iteration")
    with pytest.raises((OSError, ClientDisconnect)):
        await response({"type": "http", "asgi": {"spec_version": "2.4"}}, receive, send)
    assert closed.is_set()
    assert_empty(gateway.admission)


@pytest.mark.asyncio
async def test_64_same_key_waiters_cap_and_last_timeout_detaches_producer():
    controller = gateway.admission
    entered = asyncio.Event()
    closed = asyncio.Event()
    async def producer():
        try:
            entered.set()
            await asyncio.Event().wait()
        finally:
            closed.set()
    tasks = [asyncio.create_task(controller.run_singleflight("same", producer, 8)) for _ in range(64)]
    await entered.wait()
    await until(lambda: controller.total_waiters == 64)
    assert len(controller._producers) == 1
    with pytest.raises(HTTPException) as error:
        await controller.run_singleflight("same", producer, 8)
    assert error.value.status_code == 503
    assert error.value.headers == {"Retry-After": "1"}
    for task in tasks:
        task.cancel()
    await asyncio.gather(*tasks, return_exceptions=True)
    assert closed.is_set()
    assert_empty(controller)

    with pytest.raises(HTTPException) as error:
        await controller.run_singleflight("deadline", producer, 0.01)
    assert error.value.status_code == 504
    assert_empty(controller)


@pytest.mark.asyncio
async def test_producer_cap_reserved_before_tasks_are_created():
    controller = gateway.admission
    async def producer():
        await asyncio.Event().wait()
    tasks = [asyncio.create_task(controller.run_singleflight(str(index), producer, 8)) for index in range(18)]
    await until(lambda: len(controller._producers) == 18)
    assert controller.active_count == 2
    assert controller.waiting_count == 16
    with pytest.raises(HTTPException) as error:
        await controller.run_singleflight("nineteenth", producer, 8)
    assert error.value.status_code == 503
    assert len(controller._producers) == 18
    for task in tasks:
        task.cancel()
    await asyncio.gather(*tasks, return_exceptions=True)
    assert_empty(controller)


@pytest.mark.asyncio
async def test_one_buffered_deadline_includes_admission_and_upstream(monkeypatch):
    controller = gateway.admission
    budgets = []
    closed = asyncio.Event()
    async def producer(**kwargs):
        budgets.append(kwargs["timeout_seconds"])
        try:
            await asyncio.Event().wait()
        finally:
            closed.set()
    monkeypatch.setattr(gateway.edge_provider, "synthesize", producer)
    slot_a = controller.acquire_slot(8)
    slot_b = controller.acquire_slot(8)
    await slot_a.__aenter__()
    await slot_b.__aenter__()
    request = asyncio.create_task(gateway.synthesize(SynthesizeRequest(text="total budget", timeout_ms=100, cacheable=False)))
    await until(lambda: controller.waiting_count == 1)
    # The only wall-time dependency is the budget under test, not a race/speed assertion.
    await asyncio.sleep(0.05)
    await slot_a.__aexit__(None, None, None)
    try:
        with pytest.raises(HTTPException) as error:
            await request
        assert error.value.status_code == 504
        assert len(budgets) == 1 and 0 < budgets[0] < 0.075
        assert closed.is_set()
    finally:
        await slot_b.__aexit__(None, None, None)
    assert_empty(controller)


@pytest.mark.asyncio
async def test_stream_total_budget_expires_despite_regular_chunks_and_never_caches_partial(monkeypatch):
    closed = asyncio.Event()
    async def stream(**kwargs):
        try:
            yield b"first"
            for _ in range(100):
                await asyncio.sleep(0.01)
                yield b"next"
        finally:
            closed.set()
    monkeypatch.setattr(gateway.edge_provider, "stream", stream)
    request = SynthesizeRequest(text="regular but endless", timeout_ms=50)
    response = await gateway.synthesize_stream(request)
    chunks = []
    with pytest.raises((HTTPException, TimeoutError)):
        async for chunk in response.body_iterator:
            chunks.append(chunk)
    assert 1 <= len(chunks) < 100
    assert closed.is_set()
    assert_empty(gateway.admission)
    key = gateway.synthesis_parameters(request)[4]
    assert await gateway.cache.get(key) is None


@pytest.mark.asyncio
async def test_first_byte_budget_includes_admission(monkeypatch):
    controller = gateway.admission
    closed = asyncio.Event()
    async def stream(**kwargs):
        try:
            await asyncio.Event().wait()
            yield b"late"
        finally:
            closed.set()
    monkeypatch.setattr(gateway.edge_provider, "stream", stream)
    async with AsyncExitStack() as stack:
        first = await stack.enter_async_context(controller.acquire_slot(8))
        second_slot = controller.acquire_slot(8)
        await second_slot.__aenter__()
        task = asyncio.create_task(gateway.synthesize_stream(SynthesizeRequest(text="first byte total", timeout_ms=80, cacheable=False)))
        await until(lambda: controller.waiting_count == 1)
        await asyncio.sleep(0.04)
        await second_slot.__aexit__(None, None, None)
        with pytest.raises(HTTPException) as error:
            await task
        assert error.value.status_code == 504
        assert closed.is_set()
    assert_empty(controller)


@pytest.mark.asyncio
@pytest.mark.parametrize("value", [0, -1, 8001, True, 1.5, None])
async def test_timeout_schema_rejects_outside_total_budget(value):
    async with AsyncClient(transport=ASGITransport(app=gateway.app), base_url="http://test") as client:
        response = await client.post("/synthesize", json={"text": "budget", "timeout_ms": value})
    assert response.status_code == 422


@pytest.mark.asyncio
async def test_stream_size_cap_never_caches_partial(monkeypatch):
    async def stream(**kwargs):
        yield b"x" * gateway.MAX_AUDIO_BYTES
        yield b"extra"
    monkeypatch.setattr(gateway.edge_provider, "stream", stream)
    request = SynthesizeRequest(text="over cap")
    response = await gateway.synthesize_stream(request)
    with pytest.raises(RuntimeError, match="TTS_AUDIO_TOO_LARGE"):
        async for _ in response.body_iterator:
            pass
    assert_empty(gateway.admission)
    assert await gateway.cache.get(gateway.synthesis_parameters(request)[4]) is None


@pytest.mark.asyncio
@pytest.mark.parametrize("failure", ["first-byte", "idle", "cap", "cancel"])
async def test_buffered_provider_closes_generator_on_every_failure(monkeypatch, failure):
    closed = asyncio.Event()
    started = asyncio.Event()
    async def stream(**kwargs):
        try:
            started.set()
            if failure in ("first-byte", "cancel"):
                await asyncio.Event().wait()
            yield b"x" * gateway.MAX_AUDIO_BYTES if failure == "cap" else b"first"
            if failure == "cap":
                yield b"extra"
            else:
                await asyncio.Event().wait()
        finally:
            closed.set()
    monkeypatch.setattr(gateway.edge_provider, "stream", stream)
    monkeypatch.setattr(config, "edge_stream_initial_timeout", 0.01)
    monkeypatch.setattr(config, "edge_stream_idle_timeout", 0.01)
    if failure == "cancel":
        task = asyncio.create_task(gateway.edge_provider.synthesize("cancel", timeout_seconds=1))
        await started.wait()
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
    else:
        with pytest.raises(ValueError if failure == "cap" else TimeoutError):
            await gateway.edge_provider.synthesize("provider bounds", timeout_seconds=1)
    assert closed.is_set()


@pytest.mark.asyncio
async def test_cancelling_edge_wrapper_closes_actual_upstream_generator(monkeypatch):
    from app.providers import edge
    closed = asyncio.Event()
    class Communication:
        def __init__(self, **kwargs):
            assert kwargs["voice"] == "vi-VN-NamMinhNeural"
        async def stream(self):
            try:
                yield {"type": "audio", "data": b"first"}
                await asyncio.Event().wait()
            finally:
                closed.set()
    monkeypatch.setattr(edge.edge_tts, "Communicate", Communication)
    generator = gateway.edge_provider.stream("exact voice", "vi-VN-NamMinhNeural")
    assert await anext(generator) == b"first"
    await generator.aclose()
    assert closed.is_set()
