package migrations

import _ "embed"

// HistoryJobQueueSQL embeds the SQL migration for schema version 8.
//
//go:embed 008_history_job_queue.sql
var HistoryJobQueueSQL string
