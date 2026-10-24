"""Local VieNeu CPU configuration boundaries."""
import pytest
from pydantic import ValidationError

from app.config import Config


def test_vieneu_threads_default_to_two(monkeypatch):
    monkeypatch.delenv("TTS_VIENEU_THREADS", raising=False)
    assert Config().vieneu_threads == 2


@pytest.mark.parametrize("value", [1, 2, 3, 4])
def test_vieneu_threads_accept_supported_cpu_limits(monkeypatch, value):
    monkeypatch.setenv("TTS_VIENEU_THREADS", f" {value} ")
    assert Config().vieneu_threads == value
    assert Config(vieneu_threads=value).vieneu_threads == value


@pytest.mark.parametrize("value", ["0", "5", "-1", "2.0", "invalid", ""])
def test_vieneu_threads_reject_invalid_environment(monkeypatch, value):
    monkeypatch.setenv("TTS_VIENEU_THREADS", value)
    with pytest.raises(ValueError, match="TTS_VIENEU_THREADS"):
        Config()


@pytest.mark.parametrize("value", [0, 5, -1, 2.0, True, "2"])
def test_vieneu_threads_reject_invalid_explicit_settings(value):
    with pytest.raises(ValidationError):
        Config(vieneu_threads=value)


def test_retired_provider_environment_is_not_read(monkeypatch):
    monkeypatch.setenv("PIPER_WORKERS", "not-an-integer")
    monkeypatch.setenv("TTS_EDGE_INITIAL_TIMEOUT", "not-a-number")
    monkeypatch.setenv("TTS_EDGE_IDLE_TIMEOUT", "not-a-number")
    monkeypatch.setenv("TTS_VIENEU_THREADS", "2")
    settings = Config()
    assert settings.vieneu_threads == 2
    assert not any(name.startswith(("piper_", "edge_")) for name in Config.model_fields)
