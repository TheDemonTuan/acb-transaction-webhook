"""Data models and schemas for fixed-provider TTS."""
from typing import Literal
from pydantic import BaseModel, Field, model_validator

PIPER_VOICE = "vi-VN-vais1000-medium"
EDGE_VOICES = {"vi-VN-HoaiMyNeural", "vi-VN-NamMinhNeural"}


class SynthesizeRequest(BaseModel):
    text: str = Field(..., min_length=1, max_length=600)
    voice: Literal["vi-VN-vais1000-medium", "vi-VN-HoaiMyNeural", "vi-VN-NamMinhNeural"] = PIPER_VOICE
    rate: str = Field("+0%", pattern=r"^[+-][0-9]{1,3}%$")
    pitch: str = Field("+0Hz", pattern=r"^[+-][0-9]{1,2}Hz$")
    cacheable: bool = True
    provider_mode: Literal["PIPER_ONLY", "EDGE_ONLY"] = "PIPER_ONLY"
    timeout_ms: int = Field(8000, ge=1, le=8000, strict=True)

    @model_validator(mode="after")
    def validate_profile(self):
        if not -25 <= int(self.rate[:-1]) <= 100 or not -50 <= int(self.pitch[:-2]) <= 50:
            raise ValueError("Unsupported rate/pitch")
        if self.provider_mode == "PIPER_ONLY":
            if self.voice != PIPER_VOICE or int(self.pitch[:-2]) != 0:
                raise ValueError("Piper requires its fixed voice and zero pitch")
        elif self.voice not in EDGE_VOICES:
            raise ValueError("Edge requires an Edge voice")
        return self


class VoiceItem(BaseModel):
    id: str
    name: str
    gender: str
    provider: str


class ProviderHealth(BaseModel):
    available: bool


class HealthResponse(BaseModel):
    status: str
    primary: str
    edge: ProviderHealth
    piper: ProviderHealth


class ErrorResponse(BaseModel):
    code: str
    error: str
