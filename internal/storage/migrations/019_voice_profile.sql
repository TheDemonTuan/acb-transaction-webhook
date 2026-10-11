-- Migration 019: Voice Profile, amount bands and audio settings
ALTER TABLE voice_settings ADD COLUMN rate REAL NOT NULL DEFAULT 1.25;
ALTER TABLE voice_settings ADD COLUMN pitch_hz INTEGER NOT NULL DEFAULT 0;
ALTER TABLE voice_settings ADD COLUMN amount_bands_json TEXT NOT NULL DEFAULT '[{"id":"small","fromVnd":"1","toVnd":"50000","styleId":"classic"},{"id":"medium","fromVnd":"50000","toVnd":"200000","styleId":"classic"},{"id":"large","fromVnd":"200000","toVnd":"1000000","styleId":"classic"},{"id":"celebrate","fromVnd":"1000000","toVnd":null,"styleId":"classic"}]';
ALTER TABLE voice_settings ADD COLUMN chime_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE voice_settings ADD COLUMN loading_sound_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE voice_settings ADD COLUMN fun_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE voice_settings ADD COLUMN include_description INTEGER NOT NULL DEFAULT 0;
ALTER TABLE voice_settings ADD COLUMN custom_template TEXT NOT NULL DEFAULT '';

-- Data migration: migrate ONLINE_AUTO to EDGE_ONLY, online_fallback to 0, increment revision once
UPDATE voice_settings
SET provider_mode = CASE WHEN provider_mode = 'ONLINE_AUTO' THEN 'EDGE_ONLY' ELSE provider_mode END,
    online_fallback = 0,
    revision = revision + 1;
