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

// AuthRecoverySQL embeds schema version 12 automatic authentication recovery.
//
//go:embed 012_auth_recovery.sql
var AuthRecoverySQL string

// TelegramSessionControlSQL embeds schema version 13 durable session consent.
//
//go:embed 013_telegram_session_control.sql
var TelegramSessionControlSQL string

// PollRowsMatchedSQL embeds schema version 14 nullable transaction-day poll counts.
//
//go:embed 014_poll_rows_matched.sql
var PollRowsMatchedSQL string

// PayOSPaymentOrdersSQL embeds schema version 15 durable payOS payment orders.
//
//go:embed 015_payos_payment_orders.sql
var PayOSPaymentOrdersSQL string

// PaymentProviderConfigSQL embeds schema version 16 encrypted web-managed payOS configuration.
//
//go:embed 016_payment_provider_config.sql
var PaymentProviderConfigSQL string

// SePayStoreSQL embeds schema version 17 encrypted SePay Telegram evidence.
//
//go:embed 017_sepay_store.sql
var SePayStoreSQL string

// SePayManagedConfigSQL embeds schema version 18 encrypted admin configuration.
//
//go:embed 018_sepay_managed_config.sql
var SePayManagedConfigSQL string
