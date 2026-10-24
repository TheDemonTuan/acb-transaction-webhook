"""Data models and schemas for the fixed local VieNeu profile."""
from typing import Literal, Optional
from pydantic import BaseModel, Field, model_validator

VIENEU_VOICE = "vi-VN-vieneu-v3-turbo-mai-anh-v1"


class SynthesizeRequest(BaseModel):
    text: str = Field(..., min_length=1, max_length=600)
    voice: Literal["vi-VN-vieneu-v3-turbo-mai-anh-v1"] = VIENEU_VOICE
    rate: str = Field("+0%", pattern=r"^[+-][0-9]{1,3}%$")
    pitch: str = Field("+0Hz", pattern=r"^[+-][0-9]{1,2}Hz$")
    cacheable: bool = True
    provider_mode: Literal["VIENEU_ONLY"] = "VIENEU_ONLY"
    timeout_ms: int = Field(8000, ge=1, le=8000, strict=True)

    @model_validator(mode="after")
    def validate_profile(self):
        if int(self.rate[:-1]) != 0 or int(self.pitch[:-2]) != 0:
            raise ValueError("VieNeu requires zero rate and pitch")
        self.rate = "+0%"
        self.pitch = "+0Hz"
        return self


class VoiceItem(BaseModel):
    id: str
    name: str
    gender: str
    provider: str


class ProviderHealth(BaseModel):
    available: bool
    workerCount: Optional[int] = None
    activeWorkers: Optional[int] = None
    idleWorkers: Optional[int] = None

class HealthResponse(BaseModel):
    status: Literal["ok", "degraded"]
    primary: Literal["vieneu"]
    vieneu: ProviderHealth

class ErrorResponse(BaseModel):
    code: str
    error: str
