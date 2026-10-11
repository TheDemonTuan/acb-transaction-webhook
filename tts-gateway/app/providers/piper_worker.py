"""Persistent, single-request CPU worker; stdout is a private framed protocol."""
import json
import struct
import sys
from pathlib import Path

MAX_AUDIO_BYTES = 4 * 1024 * 1024


def send(kind, payload=b""):
    sys.stdout.buffer.write(struct.pack("!BI", kind, len(payload)))
    sys.stdout.buffer.write(payload)
    sys.stdout.buffer.flush()


def load_voice(model_path):
    import onnxruntime
    from piper import PiperVoice
    from piper.config import PiperConfig, PhonemeType

    path = Path(model_path)
    with Path(str(path) + ".json").open(encoding="utf-8") as source:
        config = PiperConfig.from_dict(json.load(source))
    # This fixed Vietnamese model needs only bundled espeak; never download G2P.
    if config.phoneme_type != PhonemeType.ESPEAK or config.espeak_voice != "vi":
        raise ValueError("Unsupported fixed model phonemizer")
    options = onnxruntime.SessionOptions()
    options.intra_op_num_threads = 1
    options.enable_cpu_mem_arena = False
    options.enable_mem_pattern = False
    options.inter_op_num_threads = 1
    options.execution_mode = onnxruntime.ExecutionMode.ORT_SEQUENTIAL
    session = onnxruntime.InferenceSession(str(path), sess_options=options, providers=["CPUExecutionProvider"])
    return PiperVoice(session=session, config=config)


def stream_encode(voice, text, rate):
    import lameenc
    from piper import SynthesisConfig

    encoder = lameenc.Encoder()
    encoder.set_bit_rate(64)
    encoder.set_in_sample_rate(voice.config.sample_rate)
    encoder.silence()
    encoder.set_out_sample_rate(24000)
    encoder.set_channels(1)
    encoder.set_quality(2)

    total_bytes = 0
    samples = 0

    for chunk in voice.synthesize(text, SynthesisConfig(length_scale=1.0 / rate)):
        samples += len(chunk.audio_int16_bytes) // 2
        if samples > voice.config.sample_rate * 120:
            raise ValueError("Audio duration exceeds limit")
        encoded = encoder.encode(chunk.audio_int16_bytes)
        if encoded:
            total_bytes += len(encoded)
            if total_bytes > MAX_AUDIO_BYTES:
                raise ValueError("Audio exceeds limit")
            for offset in range(0, len(encoded), 16384):
                yield encoded[offset:offset + 16384]

    flushed = encoder.flush()
    total_bytes += len(flushed)
    if total_bytes > MAX_AUDIO_BYTES:
        raise ValueError("Audio exceeds limit")
    for offset in range(0, len(flushed), 16384):
        yield flushed[offset:offset + 16384]

    if total_bytes == 0:
        raise ValueError("Invalid audio size: 0 bytes generated")

def main():
    voice = load_voice(sys.argv[1])
    # Warm phonemizer at readiness, not on first customer request.
    voice.phonemize("Đã nhận tiền.")
    send(0)
    for line in sys.stdin.buffer:
        try:
            request = json.loads(line)
            rate = request["rate"]
            if not 0.75 <= rate <= 2 or not isinstance(request["text"], str) or not 0 < len(request["text"]) <= 600:
                raise ValueError("Invalid request")
            for chunk in stream_encode(voice, request["text"], rate):
                send(1, chunk)
            send(3)
        except Exception:
            # Do not expose text, filesystem paths, or provider internals.
            send(2)


if __name__ == "__main__":
    main()
