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

// RecoveryRunsSQL embeds the SQL migration for schema version 10.
//
//go:embed 010_recovery_runs.sql
var RecoveryRunsSQL string

// RecoveryRunPlanSQL embeds the SQL migration for schema version 11.
//
//go:embed 011_recovery_run_plan.sql
var RecoveryRunPlanSQL string
