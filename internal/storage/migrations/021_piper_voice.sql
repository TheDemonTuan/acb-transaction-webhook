-- Cut over existing server synthesis profiles to the licensed self-hosted voice.
-- Historical edge_voice remains the storage column for the generic voiceId.
-- Preserve rate, bands, templates, and playback preferences; browser mode is explicit.
UPDATE voice_settings
SET provider_mode = 'PIPER_ONLY',
    edge_voice = 'vi-VN-vais1000-medium',
    pitch_hz = 0,
    online_fallback = 0,
    revision = revision + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE provider_mode = 'EDGE_ONLY';
