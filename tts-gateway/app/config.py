"""Configuration for TTS Gateway sidecar."""
import os
from pydantic import BaseModel, Field
def _parse_vieneu_threads() -> int:
    raw = os.getenv("TTS_VIENEU_THREADS", "2").strip()
    try:
        val = int(raw)
        if 1 <= val <= 4:
            return val
    except ValueError:
        pass
    raise ValueError(f"TTS_VIENEU_THREADS must be an integer between 1 and 4, got: {raw!r}")

class Config(BaseModel):
    vieneu_threads: int = Field(default_factory=_parse_vieneu_threads, ge=1, le=4, strict=True)
    host: str = os.getenv("TTS_HOST", "0.0.0.0")
    port: int = int(os.getenv("TTS_PORT", "8081"))
    internal_token: str = os.getenv("TTS_INTERNAL_TOKEN", "")
    token_file: str = os.getenv("TTS_INTERNAL_TOKEN_FILE", "")
    require_auth: bool = os.getenv("TTS_REQUIRE_AUTH", "").lower() in ("1", "true", "yes")
    max_text_length: int = 600
    cache_max_items: int = 256
    cache_max_bytes: int = int(os.getenv("TTS_CACHE_MAX_BYTES", str(32 * 1024 * 1024)))  # 32MB default
    cache_ttl_seconds: int = 600  # 10 minutes

    def get_token(self) -> str:
        if self.internal_token:
            return self.internal_token
        if self.token_file and os.path.exists(self.token_file):
            try:
                with open(self.token_file, "r", encoding="utf-8") as f:
                    return f.read().strip()
            except Exception:
                return ""
        return ""

config = Config()
