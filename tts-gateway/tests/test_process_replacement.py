"""Real subprocess lifecycle coverage using framing only, not model/audio evidence."""
import asyncio
import sys

import pytest
from fastapi import HTTPException

from app.admission import AdmissionController
from app.providers.process import ProcessTTSProvider


async def start_protocol_pool(monkeypatch, *, error=False):
    launch = asyncio.create_subprocess_exec
    workers = []
    script = (
        "import struct,sys; out=sys.stdout.buffer; "
        "out.write(struct.pack('!BI',0,0)); out.flush(); "
        "sys.stdin.buffer.readline(); "
        + ("out.write(struct.pack('!BI',2,0)); out.flush(); " if error else "")
        + "sys.stdin.buffer.read()"
    )

    async def protocol_launch(*args, **kwargs):
        worker = await launch(sys.executable, "-c", script, **kwargs)
        workers.append(worker)
        return worker

    monkeypatch.setattr(asyncio, "create_subprocess_exec", protocol_launch)
    provider = ProcessTTSProvider("framing-only", ())
    await provider.start()
    return provider, workers


@pytest.mark.asyncio
@pytest.mark.parametrize("failure", ["cancel", "timeout", "error"])
async def test_live_inference_replacement_owns_slot_until_ready(monkeypatch, failure):
    provider, workers = await start_protocol_pool(monkeypatch, error=failure == "error")
    controller = AdmissionController()
    original = workers[0]
    reap_started = asyncio.Event()
    release_reap = asyncio.Event()
    warming = asyncio.Event()
    release_ready = asyncio.Event()
    wait_closed = original.stdin.wait_closed
    read = provider._read
    task = None

    async def delayed_close():
        reap_started.set()
        await release_reap.wait()
        await wait_closed()

    async def delayed_ready(worker):
        if worker is not original:
            warming.set()
            await release_ready.wait()
        return await read(worker)

    monkeypatch.setattr(original.stdin, "wait_closed", delayed_close)
    monkeypatch.setattr(provider, "_read", delayed_ready)

    async def produce():
        timeout = 0.02 if failure == "timeout" else 8
        return await anext(provider.stream_request({}, timeout_seconds=timeout))

    try:
        task = asyncio.create_task(controller.run_singleflight("live-replacement", produce, 8))
        while provider.idle_count:
            await asyncio.sleep(0)
        if failure == "cancel":
            task.cancel()
        await asyncio.wait_for(reap_started.wait(), 5)
        assert controller.active_count == 1 and not task.done()
        assert len(workers) == 1 and not provider._maintenance
        release_reap.set()
        expected = asyncio.CancelledError if failure == "cancel" else HTTPException
        with pytest.raises(expected) as raised:
            await task
        if failure != "cancel":
            assert raised.value.status_code == (504 if failure == "timeout" else 502)
        await asyncio.wait_for(warming.wait(), 5)
        assert original.returncode is not None and original.stdout.at_eof()
        assert original.stdin.is_closing()
        assert controller.active_count == controller.total_waiters == 0
        assert provider.idle_count == 0 and not provider.available
        assert len(provider._maintenance) == 1 and len(workers) == 2
        assert workers[1].returncode is None
        # A new request cannot steal the warming slot or launch another worker.
        with pytest.raises(TimeoutError):
            await anext(provider.stream_request({}, timeout_seconds=0.01))
        assert len(workers) == 2
        release_ready.set()
        await asyncio.gather(*provider._maintenance)
        assert provider.available and provider.idle_count == 1
        assert not provider._maintenance
    finally:
        release_reap.set()
        release_ready.set()
        if task is not None and not task.done():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        await provider.close()
    assert all(worker.returncode is not None for worker in workers)


@pytest.mark.asyncio
async def test_close_cancels_and_reaps_warming_replacement(monkeypatch):
    provider, workers = await start_protocol_pool(monkeypatch, error=True)
    warming = asyncio.Event()
    close_started = asyncio.Event()
    release_close = asyncio.Event()
    read = provider._read
    closing = None

    async def delayed_ready(worker):
        if worker is not workers[0]:
            warming.set()
            await asyncio.Event().wait()
        return await read(worker)

    monkeypatch.setattr(provider, "_read", delayed_ready)
    try:
        with pytest.raises(RuntimeError, match="synthesis error"):
            await anext(provider.stream_request({}))
        await asyncio.wait_for(warming.wait(), 5)
        replacement = workers[1]
        wait_closed = replacement.stdin.wait_closed

        async def delayed_close():
            close_started.set()
            await release_close.wait()
            await wait_closed()

        monkeypatch.setattr(replacement.stdin, "wait_closed", delayed_close)
        closing = asyncio.create_task(provider.close())
        await asyncio.wait_for(close_started.wait(), 5)
        for _ in range(2):
            closing.cancel()
            await asyncio.sleep(0)
            assert not closing.done() and provider._maintenance
        release_close.set()
        await closing
        assert not provider._maintenance and provider._workers == [None]
        assert provider.idle_count == 0 and not provider.available
        assert all(worker.returncode is not None for worker in workers)
        assert all(worker.stdin.is_closing() and worker.stdout.at_eof() for worker in workers)
    finally:
        release_close.set()
        if closing is not None:
            await asyncio.gather(closing, return_exceptions=True)
        await provider.close()


@pytest.mark.asyncio
async def test_failed_replacement_startup_is_reaped_and_observed(monkeypatch, caplog):
    provider, workers = await start_protocol_pool(monkeypatch, error=True)
    read = provider._read

    async def invalid_ready(worker):
        if worker is not workers[0]:
            raise RuntimeError("replacement READY failed")
        return await read(worker)

    monkeypatch.setattr(provider, "_read", invalid_ready)
    try:
        with pytest.raises(RuntimeError, match="synthesis error"):
            await anext(provider.stream_request({}))
        await asyncio.gather(*provider._maintenance)
        assert not provider._maintenance and provider._workers == [None]
        assert not provider.available and provider.idle_count == 1
        assert len(workers) == 2 and all(worker.returncode is not None for worker in workers)
        assert "Process worker replacement failed for slot 0" in caplog.text
    finally:
        await provider.close()


@pytest.mark.asyncio
async def test_close_joins_inflight_replacement_launch(monkeypatch):
    provider, workers = await start_protocol_pool(monkeypatch, error=True)
    launched = asyncio.Event()
    release_launch = asyncio.Event()
    launch = asyncio.create_subprocess_exec
    closing = None

    async def delayed_launch(*args, **kwargs):
        worker = await launch(*args, **kwargs)
        launched.set()
        await release_launch.wait()
        return worker

    monkeypatch.setattr(asyncio, "create_subprocess_exec", delayed_launch)
    try:
        with pytest.raises(RuntimeError, match="synthesis error"):
            await anext(provider.stream_request({}))
        await asyncio.wait_for(launched.wait(), 5)
        assert len(workers) == 2 and provider._workers == [None]
        closing = asyncio.create_task(provider.close())
        # Observe cancellation at the maintenance task, not a timing assumption.
        maintenance = next(iter(provider._maintenance))
        while not maintenance.cancelling():
            await asyncio.sleep(0)
        assert not closing.done() and workers[1].returncode is None
        release_launch.set()
        await closing
        assert not provider._maintenance and provider._workers == [None]
        assert provider.idle_count == 0 and not provider.available
        assert all(worker.returncode is not None for worker in workers)
        assert all(worker.stdin.is_closing() and worker.stdout.at_eof() for worker in workers)
    finally:
        release_launch.set()
        if closing is not None:
            await asyncio.gather(closing, return_exceptions=True)
        await provider.close()
