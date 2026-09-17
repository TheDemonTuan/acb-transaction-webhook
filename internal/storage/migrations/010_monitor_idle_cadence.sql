-- Migration 010: Update default monitor schedule cadence from legacy 3-10s to idle 20-30s.
-- Scoped strictly to the singleton row when it matches the uncustomized legacy default profile.
-- Custom operator configurations (e.g. customized min/max or non-default modes) remain intact.
UPDATE monitor_settings
   SET windows_json = json_replace(windows_json,
         '$[0].profile.minSeconds', 20,
         '$[0].profile.maxSeconds', 30),
       revision   = revision + 1,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE id = 'singleton'
   AND json_extract(windows_json, '$[0].profile.mode')       = 'REALTIME'
   AND json_extract(windows_json, '$[0].profile.minSeconds') = 3
   AND json_extract(windows_json, '$[0].profile.maxSeconds') = 10;
