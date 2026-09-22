package migrations

import _ "embed"

// HistoryJobQueueSQL embeds the SQL migration for schema version 8.
//
//go:embed 008_history_job_queue.sql
var HistoryJobQueueSQL string

// DeploymentControlSQL embeds the SQL migration for schema version 9.
//
//go:embed 009_deployment_control.sql
var DeploymentControlSQL string

// MonitorIdleCadenceSQL embeds the historical schema version 10 migration.
//
//go:embed 010_monitor_idle_cadence.sql
var MonitorIdleCadenceSQL string

// RecoveryRunsSQL embeds the schema version 11 recovery migration.
//
//go:embed 011_recovery_runs.sql
var RecoveryRunsSQL string
