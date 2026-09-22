-- Migration 011: durable recovery plan and resume day.
ALTER TABLE recovery_runs ADD COLUMN reason TEXT NOT NULL DEFAULT '';
ALTER TABLE recovery_runs ADD COLUMN range_from TEXT NOT NULL DEFAULT '';
ALTER TABLE recovery_runs ADD COLUMN range_to TEXT NOT NULL DEFAULT '';
ALTER TABLE recovery_runs ADD COLUMN next_day TEXT NOT NULL DEFAULT '';
