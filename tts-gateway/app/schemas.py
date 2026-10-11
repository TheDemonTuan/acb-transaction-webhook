"""Data models and schemas for TTS Gateway."""
from typing import Literal
from pydantic import BaseModel, Field

class SynthesizeRequest(BaseModel):
    text: str = Field(..., min_length=1, max_length=600)
    voice: Literal["vi-VN-HoaiMyNeural", "vi-VN-NamMinhNeural"] = "vi-VN-HoaiMyNeural"
    rate: str = Field("+0%", pattern=r"^[+-][0-9]{1,3}%$")
    pitch: str = Field("+0Hz", pattern=r"^[+-][0-9]{1,2}Hz$")
    cacheable: bool = True
    provider_mode: Literal["EDGE_ONLY"] = "EDGE_ONLY"
    timeout_ms: int = Field(8000, ge=1, le=8000, strict=True)
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
    gtts: ProviderHealth

class ErrorResponse(BaseModel):
    code: str
    error: str
