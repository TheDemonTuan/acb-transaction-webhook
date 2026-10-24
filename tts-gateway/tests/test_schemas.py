"""VieNeu-only request and health model contract coverage."""
import pytest
from pydantic import ValidationError

from app.schemas import HealthResponse, ProviderHealth, SynthesizeRequest, VIENEU_VOICE


def test_request_defaults_to_fixed_vieneu_profile():
    request = SynthesizeRequest(text="Đã nhận năm trăm nghìn đồng.")
    assert request.voice == VIENEU_VOICE
    assert request.provider_mode == "VIENEU_ONLY"
    assert request.rate == "+0%" and request.pitch == "+0Hz"
    assert request.timeout_ms == 8000


@pytest.mark.parametrize("overrides", [
    {"voice": "vi-VN-vais1000-medium"},
    {"voice": "vi-VN-HoaiMyNeural"},
    {"voice": "vi-VN-NamMinhNeural"},
    {"voice": "unknown"},
    {"provider_mode": "PIPER_ONLY"},
    {"provider_mode": "EDGE_ONLY"},
    {"provider_mode": "ONLINE_AUTO"},
    {"rate": "+1%"},
    {"rate": "-1%"},
    {"rate": "+100%"},
    {"rate": "0"},
    {"rate": 0},
    {"pitch": "+1Hz"},
    {"pitch": "-1Hz"},
    {"pitch": "0"},
    {"pitch": 0},
    {"text": ""},
    {"text": "x" * 601},
    {"timeout_ms": 0},
    {"timeout_ms": 8001},
    {"timeout_ms": True},
    {"timeout_ms": 1.5},
    {"timeout_ms": "8000"},
])
def test_request_rejects_non_vieneu_profile_and_invalid_limits(overrides):
    with pytest.raises(ValidationError):
        SynthesizeRequest(**{"text": "Đã nhận tiền.", **overrides})


@pytest.mark.parametrize("rate,pitch", [("+0%", "+0Hz"), ("-0%", "-0Hz"), ("+000%", "+00Hz")])
def test_numeric_zero_is_canonicalized_for_provider_and_cache(rate, pitch):
    request = SynthesizeRequest(text="Đã nhận tiền.", rate=rate, pitch=pitch)
    assert request.rate == "+0%" and request.pitch == "+0Hz"


@pytest.mark.parametrize("status", ["ok", "degraded"])
def test_health_exposes_only_local_vieneu(status):
    health = HealthResponse(status=status, primary="vieneu", vieneu=ProviderHealth(available=status == "ok"))
    assert set(health.model_dump()) == {"status", "primary", "vieneu"}


@pytest.mark.parametrize("overrides", [{"status": "unknown"}, {"primary": "piper"}, {"primary": "edge"}])
def test_health_rejects_retired_primary_and_unknown_status(overrides):
    with pytest.raises(ValidationError):
        HealthResponse(**{"status": "ok", "primary": "vieneu", "vieneu": ProviderHealth(available=True), **overrides})
