UPDATE voice_settings
SET provider_mode = 'VIENEU_ONLY',
    edge_voice = 'vi-VN-vieneu-v3-turbo-mai-anh-v1',
    rate = 1.00,
    pitch_hz = 0,
    online_fallback = 0,
    revision = revision + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE provider_mode <> 'VIENEU_ONLY'
   OR edge_voice <> 'vi-VN-vieneu-v3-turbo-mai-anh-v1'
   OR rate <> 1.00 OR pitch_hz <> 0 OR online_fallback <> 0;
