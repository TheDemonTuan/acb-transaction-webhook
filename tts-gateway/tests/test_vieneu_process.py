"""Real local model/encoder coverage; no upstream calls or fabricated audio."""
import asyncio
import os
import signal
import sys

import pytest

from app.admission import AdmissionController
from app.providers.process import ProcessTTSProvider
from app.providers.vieneu import VieneuTTSProvider
from app.schemas import SynthesizeRequest


def is_mp3(audio):
    return any(audio[index] == 255 and audio[index + 1] & 224 == 224
               and (audio[index + 2] >> 4) not in (0, 15)
               for index in range(min(len(audio) - 2, 4096)))


@pytest.mark.asyncio
@pytest.mark.skipif(os.name != "posix", reason="SIGSTOP fault injection requires POSIX; Docker smoke covers deployment")
async def test_cancellation_reaps_real_inference_before_releasing_admission():
    provider = VieneuTTSProvider()
    controller = AdmissionController()
    try:
        await provider.start()
        process = provider._workers[0]
        os.kill(process.pid, signal.SIGSTOP)
        task = asyncio.create_task(controller.run_singleflight(
            "real-stopped-worker", lambda: provider.synthesize("Đã nhận năm trăm nghìn đồng."), 8))
        while provider._idle.qsize() == 1:
            await asyncio.sleep(0)
        assert controller.active_count == 1 and process.returncode is None
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert process.returncode is not None
        assert controller.active_count == 0 and controller.total_waiters == 0
        # Recovery belongs to the pool, not the next eight-second synthesis.
        await asyncio.wait_for(
            asyncio.gather(*provider._process_pool._maintenance),
            timeout=provider._process_pool.startup_timeout + 5,
        )
        assert provider.available and provider.idle_count == 1
        assert provider._workers[0] is not process
        audio = await provider.synthesize("Đã nhận năm trăm nghìn đồng.")
        assert is_mp3(audio)
        assert provider.available and len(provider._workers) == 1
    finally:
        await provider.close()


@pytest.mark.asyncio
@pytest.mark.skipif(os.name != "posix", reason="Stopped-inference fault requires SIGSTOP")
async def test_real_http_disconnect_kills_inference(monkeypatch):
    import app.main as gateway
    from app.config import config
    from starlette.requests import Request
    monkeypatch.setattr(config, "require_auth", False)
    monkeypatch.setattr(config, "internal_token", "")
    monkeypatch.setattr(config, "token_file", "")
    provider = VieneuTTSProvider()
    controller = AdmissionController()
    monkeypatch.setattr(gateway, "vieneu_provider", provider)
    monkeypatch.setattr(gateway, "admission", controller)
    disconnected = asyncio.Event()
    async def receive():
        await disconnected.wait()
        return {"type": "http.disconnect"}
    request = Request({"type": "http", "method": "POST", "path": "/synthesize"}, receive)
    try:
        await provider.start()
        process = provider._workers[0]
        os.kill(process.pid, signal.SIGSTOP)
        task = asyncio.create_task(gateway.synthesize(
            SynthesizeRequest(text="Đã nhận năm trăm nghìn đồng.", cacheable=False), request=request))
        while provider._idle.qsize() == 1:
            await asyncio.sleep(0)
        disconnected.set()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert process.returncode is not None
        assert controller.active_count == controller.total_waiters == 0
        # Readiness must recover without another synthesis request to trigger it.
        async with asyncio.timeout(provider._process_pool.startup_timeout + 5):
            while not (await gateway.health()).vieneu.available:
                await asyncio.sleep(0.05)
        assert provider.idle_count == 1 and provider._workers[0] is not process
        assert is_mp3(await provider.synthesize("Đã nhận năm trăm nghìn đồng."))
    finally:
        await provider.close()


@pytest.mark.asyncio
async def test_consumer_cancellation_during_recovery_reaps_without_respawn(monkeypatch):
    provider = VieneuTTSProvider()
    controller = AdmissionController()
    reap_started = asyncio.Event()
    release_reap = asyncio.Event()
    producer_cancelled = asyncio.Event()
    task = None
    try:
        await provider.start()
        process = provider._workers[0]
        process.kill()
        await process.wait()
        original_wait = process.wait
        async def delayed_reap():
            # Delay only the OS-wait completion boundary of a real exited worker.
            reap_started.set()
            await release_reap.wait()
            return await original_wait()
        monkeypatch.setattr(process, "wait", delayed_reap)
        async def forbidden_respawn(index):
            pytest.fail("Cancelled recovery must not respawn or synthesize")
        monkeypatch.setattr(provider, "_spawn", forbidden_respawn)
        async def produce():
            try:
                return await provider.synthesize("Đã nhận năm trăm nghìn đồng.")
            except asyncio.CancelledError:
                producer_cancelled.set()
                raise
        task = asyncio.create_task(controller.run_singleflight("cancel-recovery", produce, 8))
        await reap_started.wait()
        task.cancel()
        # Producer cancellation must be observed before releasing the reaper.
        while controller._producers.get("cancel-recovery") is not None:
            await asyncio.sleep(0)
        assert controller.active_count == 1 and not task.done()
        release_reap.set()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert producer_cancelled.is_set()
        assert process.returncode is not None and provider._workers[0] is None
        assert provider._idle.qsize() == 1
        assert controller.active_count == controller.total_waiters == 0
    finally:
        release_reap.set()
        if task is not None and not task.done():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        await provider.close()


@pytest.mark.asyncio
@pytest.mark.parametrize("already_exited", [False, True], ids=["running", "exited"])
@pytest.mark.parametrize("propagate_cancel", [False, True], ids=["cleanup", "propagate"])
async def test_stop_closes_real_pipes_before_releasing_worker_despite_repeated_cancellation(
    monkeypatch, already_exited, propagate_cancel
):
    """Real OS pipes, including Windows Proactor pipes; no model is needed."""
    provider = ProcessTTSProvider("unused", ())
    worker = await asyncio.create_subprocess_exec(
        sys.executable,
        "-c",
        "import sys; sys.stdout.buffer.write(b'ready'); "
        "sys.stdout.buffer.flush(); sys.stdin.buffer.read()",
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
    )
    provider._workers[0] = worker
    close_started = asyncio.Event()
    release_close = asyncio.Event()
    close_cancelled = asyncio.Event()
    close_finished = asyncio.Event()
    original_wait_closed = worker.stdin.wait_closed
    task = None

    async def delayed_pipe_close():
        close_started.set()
        try:
            await release_close.wait()
            await original_wait_closed()
        except asyncio.CancelledError:
            close_cancelled.set()
            raise
        finally:
            close_finished.set()

    try:
        assert await asyncio.wait_for(worker.stdout.readexactly(5), timeout=5) == b"ready"
        if already_exited:
            worker.kill()
            await worker.wait()
        monkeypatch.setattr(worker.stdin, "wait_closed", delayed_pipe_close)
        task = asyncio.create_task(provider._stop(0, propagate_cancel=propagate_cancel))
        await asyncio.wait_for(close_started.wait(), timeout=5)
        assert worker.returncode is not None
        assert worker.stdin.is_closing() and worker.stdout.at_eof()
        for _ in range(2):
            task.cancel()
            await asyncio.sleep(0)
            assert not task.done()
            assert provider._workers[0] is worker
        assert not close_cancelled.is_set()
        release_close.set()
        if propagate_cancel:
            with pytest.raises(asyncio.CancelledError):
                await task
        else:
            await task
        assert provider._workers[0] is None
        assert worker.returncode is not None
        assert close_finished.is_set() and not close_cancelled.is_set()
    finally:
        release_close.set()
        if task is not None:
            await asyncio.gather(task, return_exceptions=True)
        await provider.close()


def test_encoder_emits_short_sentence_before_generating_next(monkeypatch):
    """Framing-only double: this is not real audio or inference timing evidence."""
    import sys
    from types import SimpleNamespace
    import numpy as np
    from app.providers.vieneu_worker import stream_encode
    encoded_sentence = b"encoded-sentence"
    flush_calls = []
    class Encoder:
        def __getattr__(self, name):
            return lambda *args: None
        def encode(self, pcm):
            return encoded_sentence
        def flush(self):
            flush_calls.append(True)
            return b"trailer"
    monkeypatch.setitem(sys.modules, "lameenc", SimpleNamespace(Encoder=Encoder))
    second_generated = []
    def sentences(text, **kwargs):
        yield np.zeros(1, dtype=np.float32)
        second_generated.append(True)
        yield np.zeros(1, dtype=np.float32)
    engine = SimpleNamespace(infer_stream=sentences)
    encoded = stream_encode(engine, "two sentences")
    assert next(encoded) == encoded_sentence
    assert not second_generated and not flush_calls
    assert list(encoded) == [encoded_sentence, b"trailer"]
    assert second_generated and flush_calls == [True]


@pytest.mark.asyncio
@pytest.mark.parametrize("ending", ["missing-end", "error", "empty-end"])
async def test_private_stream_requires_data_and_valid_end(monkeypatch, ending):
    import struct
    from types import SimpleNamespace
    provider = VieneuTTSProvider()
    stdout = asyncio.StreamReader()
    if ending != "empty-end":
        stdout.feed_data(struct.pack("!BI", 1, 5) + b"audio")
    if ending == "error":
        stdout.feed_data(struct.pack("!BI", 2, 0))
    elif ending == "empty-end":
        stdout.feed_data(struct.pack("!BI", 3, 0))
    stdout.feed_eof()
    async def drain():
        pass
    worker = SimpleNamespace(returncode=None, stdout=stdout, stdin=SimpleNamespace(write=lambda _: None, drain=drain))
    provider._workers[0] = worker
    provider._closed = False
    provider._idle.put_nowait(0)
    stopped = []
    async def stop(index, propagate_cancel=True):
        stopped.append(index)
        provider._workers[index] = None
    monkeypatch.setattr(provider, "_stop", stop)
    spawned = []
    async def spawn(index):
        spawned.append(index)
    monkeypatch.setattr(provider, "_spawn", spawn)
    with pytest.raises((RuntimeError, asyncio.IncompleteReadError)):
        async for _ in provider.stream("protocol fixture"):
            pass
    await asyncio.gather(*provider._process_pool._maintenance)
    assert stopped == spawned == [0] and provider.idle_count == 1
    await provider.close()


@pytest.mark.asyncio
async def test_worker_recovery_uses_remaining_total_deadline(monkeypatch):
    provider = VieneuTTSProvider()
    provider._closed = False
    provider._idle.put_nowait(0)
    spawning_closed = asyncio.Event()
    async def spawn(index):
        try:
            await asyncio.Event().wait()
        finally:
            spawning_closed.set()
    monkeypatch.setattr(provider, "_spawn", spawn)
    with pytest.raises(TimeoutError):
        await anext(provider.stream("recovery fixture", timeout_seconds=0.01))
    assert spawning_closed.is_set() and provider.idle_count == 1
