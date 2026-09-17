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

// MonitorIdleCadenceSQL embeds the SQL migration for schema version 10.
//
//go:embed 010_monitor_idle_cadence.sql
var MonitorIdleCadenceSQL string
