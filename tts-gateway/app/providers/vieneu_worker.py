"""Subprocess worker running VieNeu v3 Turbo ONNX for Mai Anh voice."""
import hashlib
import json
from pathlib import Path
import struct
import sys

import numpy as np

MAX_AUDIO_BYTES = 4 * 1024 * 1024
VOICE_ID = "vi-VN-vieneu-v3-turbo-mai-anh-v1"
MAI_ANH_PRESET_NAME = "Mai Anh"


def send(kind: int, payload: bytes = b""):
    sys.stdout.buffer.write(struct.pack("!BI", kind, len(payload)))
    sys.stdout.buffer.write(payload)
    sys.stdout.buffer.flush()


def seed_for_text(text: str) -> int:
    h = hashlib.sha256((VOICE_ID + "\n" + text).encode("utf-8")).digest()
    return int.from_bytes(h[:4], "big")


def stream_encode(engine, text: str):
    import lameenc

    encoder = lameenc.Encoder()
    encoder.set_bit_rate(64)
    encoder.set_in_sample_rate(48000)
    encoder.silence()
    encoder.set_out_sample_rate(24000)
    encoder.set_channels(1)
    encoder.set_quality(2)

    total_bytes = 0
    samples = 0
    generated_any_samples = False

    # Deterministic seed before generation
    seed = seed_for_text(text)
    np.random.seed(seed)

    for chunk in engine.infer_stream(
        text,
        voice=MAI_ANH_PRESET_NAME,
        temperature=0.8,
        top_k=25,
        top_p=0.95,
        repetition_penalty=1.2,
        max_new_frames=300,
        max_chars=256,
        apply_watermark=False,
    ):
        if not isinstance(chunk, np.ndarray):
            chunk = np.asarray(chunk, dtype=np.float32)

        if not np.all(np.isfinite(chunk)):
            raise ValueError("Inference produced non-finite (NaN/Inf) audio samples")

        if chunk.size == 0:
            continue

        generated_any_samples = True
        samples += chunk.size
        if samples > 48000 * 120:
            raise ValueError("Audio duration exceeds 120s limit")

        # Convert float32 [-1.0, 1.0] to signed PCM16 with clamping
        clamped = np.clip(chunk, -1.0, 1.0)
        pcm16 = (clamped * 32767.0).astype(np.int16).tobytes()

        encoded = encoder.encode(pcm16)
        if encoded:
            total_bytes += len(encoded)
            if total_bytes > MAX_AUDIO_BYTES:
                raise ValueError("Audio exceeds max limit")
            for offset in range(0, len(encoded), 16384):
                yield encoded[offset:offset + 16384]

    flushed = encoder.flush()
    if flushed:
        total_bytes += len(flushed)
        if total_bytes > MAX_AUDIO_BYTES:
            raise ValueError("Audio exceeds max limit")
        for offset in range(0, len(flushed), 16384):
            yield flushed[offset:offset + 16384]

    if not generated_any_samples or total_bytes == 0:
        raise ValueError("Utterance produced no audio data")


def main():
    if len(sys.argv) < 3:
        sys.stderr.write("Usage: vieneu_worker.py <model_root> <threads>\n")
        sys.exit(1)

    model_root = Path(sys.argv[1]).resolve()
    threads = int(sys.argv[2])

    backbone_dir = model_root / "backbone"
    onnx_update_dir = backbone_dir / "onnx_update"
    preset_file = backbone_dir / "voices_v3_turbo.json"

    if not preset_file.is_file():
        sys.stderr.write(f"Preset file not found: {preset_file}\n")
        sys.exit(1)

    with preset_file.open("r", encoding="utf-8") as f:
        presets = json.load(f).get("presets", {})
    if MAI_ANH_PRESET_NAME not in presets:
        sys.stderr.write(f"Preset {MAI_ANH_PRESET_NAME} missing in {preset_file}\n")
        sys.exit(1)

    ma_meta = presets[MAI_ANH_PRESET_NAME]
    if ma_meta.get("gender") != "female" or ma_meta.get("region") != "Bắc":
        sys.stderr.write(f"Preset {MAI_ANH_PRESET_NAME} metadata invalid: {ma_meta}\n")
        sys.exit(1)

    from vieneu import Vieneu

    engine = Vieneu(
        mode="v3turbo",
        device="cpu",
        backend="onnx",
        precision="fp32",
        backbone_repo=str(backbone_dir),
        onnx_dir=str(onnx_update_dir),
        threads=threads,
        babble_retries=0,
    )

    # Invariant: verify sess_codec_step is present for streaming decode
    if getattr(engine.engine, "sess_codec_step", None) is None:
        sys.stderr.write("Fatal: Streaming codec session (sess_codec_step) is not available\n")
        sys.exit(1)

    # Warmup utterance through the exact pipeline and discard warmup audio
    warmup_text = "Đã nhận một trăm linh năm nghìn đồng."
    for _ in stream_encode(engine, warmup_text):
        pass

    # Emit READY frame
    send(0)

    # Handle IPC loop
    for line in sys.stdin.buffer:
        try:
            req = json.loads(line)
            text = req["text"]
            voice = req["voice"]
            rate = req.get("rate", "+0%")
            pitch = req.get("pitch", "+0Hz")

            if voice != VOICE_ID:
                raise ValueError(f"Invalid voice ID: {voice}")
            if rate != "+0%" or pitch != "+0Hz":
                raise ValueError(f"Invalid rate/pitch for VieNeu: rate={rate}, pitch={pitch}")
            if not isinstance(text, str) or not 0 < len(text) <= 600:
                raise ValueError("Text length invalid")

            for chunk in stream_encode(engine, text):
                send(1, chunk)
            send(3)
        except Exception as exc:
            # Send ERROR frame without leaking text or stack trace
            send(2)


if __name__ == "__main__":
    main()
