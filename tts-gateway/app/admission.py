"""Shared bounded admission and cancellation-safe singleflight synthesis."""
import asyncio
from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import Awaitable, Callable, Optional

import anyio
from fastapi import HTTPException


def timeout_error():
    return HTTPException(status_code=504, detail="TTS_TIMEOUT")


def remaining(deadline):
    budget = deadline - asyncio.get_running_loop().time()
    if budget <= 0:
        raise timeout_error()
    return budget


@dataclass
class Reservation:
    active: bool
    released: bool = False


class AdmissionController:
    def __init__(self, max_active=2, max_waiting=16, max_total_waiters=64):
        self.max_active = max_active
        self.max_waiting = max_waiting
        self.max_total_waiters = max_total_waiters
        self.active_count = 0
        self.waiting_count = 0
        self.total_waiters = 0
        self._lock = asyncio.Lock()
        self._slot_available = asyncio.Condition(self._lock)
        self._producers = {}

    @staticmethod
    def _overload():
        return HTTPException(status_code=503, detail="TTS_OVERLOADED", headers={"Retry-After": "1"})

    async def register_waiter(self):
        async with self._lock:
            if self.total_waiters >= self.max_total_waiters:
                raise self._overload()
            self.total_waiters += 1

    async def unregister_waiter(self):
        async with self._lock:
            self.total_waiters -= 1

    def _reserve_locked(self):
        if self.active_count < self.max_active:
            self.active_count += 1
            return Reservation(active=True)
        if self.waiting_count >= self.max_waiting:
            raise self._overload()
        self.waiting_count += 1
        return Reservation(active=False)

    async def _release(self, reservation):
        async with self._lock:
            if reservation.released:
                return
            reservation.released = True
            if reservation.active:
                self.active_count -= 1
                self._slot_available.notify(1)
            else:
                self.waiting_count -= 1

    @asynccontextmanager
    async def _use_reservation(self, reservation, deadline):
        try:
            async with self._lock:
                if not reservation.active:
                    await asyncio.wait_for(
                        self._slot_available.wait_for(lambda: self.active_count < self.max_active),
                        timeout=remaining(deadline),
                    )
                    self.waiting_count -= 1
                    self.active_count += 1
                    reservation.active = True
            remaining(deadline)
            yield
        except TimeoutError:
            raise timeout_error() from None
        finally:
            with anyio.CancelScope(shield=True):
                await self._release(reservation)

    @asynccontextmanager
    async def acquire_slot(self, timeout_seconds):
        deadline = asyncio.get_running_loop().time() + timeout_seconds
        async with self._lock:
            reservation = self._reserve_locked()
        async with self._use_reservation(reservation, deadline):
            yield

    async def run_singleflight(self, key, producer_fn, timeout_seconds, disconnected=None):
        deadline = asyncio.get_running_loop().time() + timeout_seconds
        await self.register_waiter()
        entry = None
        disconnect_task = None
        disconnect_stop = asyncio.Event()
        try:
            async with self._lock:
                remaining(deadline)
                entry = self._producers.get(key)
                if entry is None:
                    # Reserve before allocating a future or task. At most 18 producers.
                    reservation = self._reserve_locked()
                    entry = ProducerEntry(key, reservation)
                    self._producers[key] = entry
                    entry.task = asyncio.create_task(self._producer_runner(entry, producer_fn, deadline))
                entry.waiters_count += 1
            try:
                if disconnected is None:
                    return await asyncio.wait_for(asyncio.shield(entry.future), timeout=remaining(deadline))
                async def watch_disconnect():
                    while not disconnect_stop.is_set():
                        if await disconnected():
                            return
                        try:
                            await asyncio.wait_for(disconnect_stop.wait(), timeout=0.025)
                        except TimeoutError:
                            pass
                disconnect_task = asyncio.create_task(watch_disconnect())
                done, _ = await asyncio.wait((entry.future, disconnect_task), timeout=remaining(deadline), return_when=asyncio.FIRST_COMPLETED)
                if entry.future in done:
                    return entry.future.result()
                if disconnect_task in done:
                    raise asyncio.CancelledError
                raise timeout_error()
            except TimeoutError:
                raise timeout_error() from None
        finally:
            with anyio.CancelScope(shield=True):
                if disconnect_task is not None:
                    # Request.is_disconnected uses an internally cancelled AnyIO
                    # scope and can consume an external task.cancel race. Signal
                    # cooperative completion instead, then join its bounded poll.
                    disconnect_stop.set()
                    await asyncio.gather(disconnect_task, return_exceptions=True)
                if entry is not None:
                    task_to_join = None
                    async with self._lock:
                        entry.waiters_count -= 1
                        if entry.waiters_count == 0:
                            task_to_join = entry.task
                            if not entry.future.done():
                                entry.abandoned = True
                                # An old finalizer cannot remove a replacement producer.
                                if self._producers.get(key) is entry:
                                    del self._producers[key]
                                if entry.started:
                                    entry.task.cancel()
                    if task_to_join is not None:
                        await asyncio.gather(task_to_join, return_exceptions=True)
                await self.unregister_waiter()

    async def _producer_runner(self, entry, producer_fn, deadline):
        entry.started = True
        try:
            if entry.abandoned:
                raise asyncio.CancelledError
            async with self._use_reservation(entry.reservation, deadline):
                # One producer budget covers both admission and complete upstream work.
                async with asyncio.timeout_at(deadline):
                    result = await producer_fn()
                if not entry.future.done():
                    entry.future.set_result(result)
        except asyncio.CancelledError:
            entry.future.cancel()
        except TimeoutError:
            if not entry.future.done():
                entry.future.set_exception(timeout_error())
        except HTTPException as error:
            if not entry.future.done():
                entry.future.set_exception(error)
        except Exception:
            if not entry.future.done():
                entry.future.set_exception(HTTPException(status_code=502, detail="TTS_UNAVAILABLE"))
        finally:
            with anyio.CancelScope(shield=True):
                await self._release(entry.reservation)
                async with self._lock:
                    if self._producers.get(entry.key) is entry:
                        del self._producers[entry.key]


class ProducerEntry:
    def __init__(self, key, reservation):
        self.key = key
        self.reservation = reservation
        self.future = asyncio.get_running_loop().create_future()
        # A last timed-out waiter can leave a completed error with no consumer.
        self.future.add_done_callback(lambda future: None if future.cancelled() else future.exception())
        self.task: Optional[asyncio.Task] = None
        self.waiters_count = 0
        self.started = False
        self.abandoned = False
