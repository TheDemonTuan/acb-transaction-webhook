const SOFT_KNEE = 0.75;
const CEILING = 10 ** (-1 / 20);
const CURVE = new Float32Array(4097);
for (let i = 0; i < CURVE.length; i++) {
  const x = i * 2 / (CURVE.length - 1) - 1;
  const magnitude = Math.abs(x);
  CURVE[i] = magnitude <= SOFT_KNEE ? x : Math.sign(x) *
    (SOFT_KNEE + (CEILING - SOFT_KNEE) * Math.tanh((magnitude - SOFT_KNEE) / (CEILING - SOFT_KNEE)));
}

/** Shared speech gain with soft peak protection, followed by the device volume. */
export function createSpeechOutput(context: BaseAudioContext, volume: number): { input: GainNode; disconnect: () => void } {
  const input = context.createGain();
  const protection = context.createWaveShaper();
  const localVolume = context.createGain();
  input.gain.value = 2;
  protection.curve = CURVE;
  protection.oversample = 'none';
  localVolume.gain.value = Math.max(0, Math.min(1, volume));
  input.connect(protection); protection.connect(localVolume); localVolume.connect(context.destination);
  let disconnected = false;
  return { input, disconnect: () => {
    if (disconnected) return;
    disconnected = true;
    input.disconnect(); protection.disconnect(); localVolume.disconnect();
  } };
}
