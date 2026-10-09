package telemetry

import (
	"fmt"
	"time"
)

type AlertLevel string

const (
	AlertInfo     AlertLevel = "INFO"
	AlertWarning  AlertLevel = "WARNING"
	AlertCritical AlertLevel = "CRITICAL"
)

const (
	AlertStaleWorker              = "ALERT_STALE_WORKER"
	AlertNotificationBacklogStuck = "ALERT_NOTIFICATION_BACKLOG_STUCK"
	AlertBackupOverdue            = "ALERT_BACKUP_OVERDUE"
	AlertRestoreDrillOverdue      = "ALERT_RESTORE_DRILL_OVERDUE"
	AlertMutationGateLocked       = "ALERT_MUTATION_GATE_LOCKED"
	AlertWrongRoleReleaseSchema   = "ALERT_WRONG_ROLE_RELEASE_SCHEMA"
	AlertRealtimeStreamDegraded   = "ALERT_REALTIME_STREAM_DEGRADED"
	AlertRealtimeFallbackRecovery = "ALERT_REALTIME_FALLBACK_RECOVERY"
)

type Alert struct {
	Name         string     `json:"name"`
	Level        AlertLevel `json:"level"`
	Active       bool       `json:"active"`
	Message      string     `json:"message"`
	Threshold    string     `json:"threshold"`
	CurrentValue any        `json:"currentValue"`
}

// EvaluateAlerts checks operational thresholds against the provided telemetry snapshot.
func EvaluateAlerts(s TelemetrySnapshot) []Alert {
	var alerts []Alert

	// Stale Worker / Worker Restart Exhaustion (>60s heartbeat or IsStale true)
	staleWorker := false
	var workerLevel AlertLevel = AlertInfo
	var workerMsg string
	if s.Singleton.IsStale || (s.Singleton.HeartbeatAt != "" && s.Singleton.HeartbeatAgeSeconds > 60) {
		staleWorker = true
		workerLevel = AlertCritical
		workerMsg = fmt.Sprintf("Worker heartbeat is stale (%.0fs ago, state: %s)", s.Singleton.HeartbeatAgeSeconds, s.Singleton.WorkerState)
	} else if s.Singleton.WorkerState != "" && s.Singleton.WorkerState != "READY" && s.Singleton.WorkerState != "DRAINING" {
		staleWorker = true
		workerLevel = AlertWarning
		workerMsg = fmt.Sprintf("Worker state unexpected: %s", s.Singleton.WorkerState)
	} else {
		workerMsg = "Worker is active and healthy"
	}
	alerts = append(alerts, Alert{
		Name:         AlertStaleWorker,
		Level:        workerLevel,
		Active:       staleWorker,
		Message:      workerMsg,
		Threshold:    "> 60s without heartbeat or state!=READY",
		CurrentValue: s.Singleton.HeartbeatAgeSeconds,
	})

	// Notification Backlog Stuck (dead letter > 10 or pending > 50)
	notifStuck := false
	var notifLevel AlertLevel = AlertInfo
	var notifMsg string
	if s.Notifications.IsBacklogStuck || s.Notifications.TotalDeadLetter > 10 || s.Notifications.TotalPending > 50 {
		notifStuck = true
		notifLevel = AlertWarning
		notifMsg = fmt.Sprintf("Notification backlog: %d dead-letter, %d pending", s.Notifications.TotalDeadLetter, s.Notifications.TotalPending)
	} else {
		notifMsg = "Notification deliveries processing normally"
	}
	alerts = append(alerts, Alert{
		Name:         AlertNotificationBacklogStuck,
		Level:        notifLevel,
		Active:       notifStuck,
		Message:      notifMsg,
		Threshold:    "dead_letter > 10 or pending > 50",
		CurrentValue: s.Notifications.TotalDeadLetter,
	})

	// Backup Overdue (>86400s / 24 hours)
	backupOverdue := false
	var backupLevel AlertLevel = AlertInfo
	var backupMsg string
	if s.BackupAndRestore.IsBackupOverdue || (s.BackupAndRestore.LastBackupAt != "" && s.BackupAndRestore.BackupAgeSeconds > 86400) {
		backupOverdue = true
		backupLevel = AlertWarning
		backupMsg = fmt.Sprintf("Backup overdue: last backup is %.1f hours old", s.BackupAndRestore.BackupAgeSeconds/3600)
	} else if s.BackupAndRestore.LastBackupAt == "" && s.Deployment.RuntimeRole != "" {
		backupOverdue = true
		backupLevel = AlertWarning
		backupMsg = "No verified backup artifact found"
	} else {
		backupMsg = "Recent backup is up-to-date"
	}
	alerts = append(alerts, Alert{
		Name:         AlertBackupOverdue,
		Level:        backupLevel,
		Active:       backupOverdue,
		Message:      backupMsg,
		Threshold:    "backup age > 24 hours (86400s)",
		CurrentValue: s.BackupAndRestore.BackupAgeSeconds,
	})

	// Restore Drill Overdue (>30 days or drill failed)
	drillOverdue := false
	var drillLevel AlertLevel = AlertInfo
	var drillMsg string
	if s.BackupAndRestore.LastRestoreDrillAt != "" {
		if !s.BackupAndRestore.LastRestoreDrillSuccess {
			drillOverdue = true
			drillLevel = AlertCritical
			drillMsg = "Last off-host disaster recovery drill failed"
		} else if s.BackupAndRestore.RestoreDrillAgeDays > 30 {
			drillOverdue = true
			drillLevel = AlertWarning
			drillMsg = fmt.Sprintf("Disaster recovery drill overdue (%.1f days since last drill)", s.BackupAndRestore.RestoreDrillAgeDays)
		} else {
			drillMsg = "Disaster recovery drill is current"
		}
	} else {
		drillMsg = "No restore drill recorded"
	}
	alerts = append(alerts, Alert{
		Name:         AlertRestoreDrillOverdue,
		Level:        drillLevel,
		Active:       drillOverdue,
		Message:      drillMsg,
		Threshold:    "restore drill age > 30 days or drill failure",
		CurrentValue: s.BackupAndRestore.RestoreDrillAgeDays,
	})

	// Mutation Gate Locked during deployment
	gateLocked := false
	var gateLevel AlertLevel = AlertInfo
	var gateMsg string
	if s.MutationGate.IsLocked {
		gateLocked = true
		gateLevel = AlertWarning
		gateMsg = fmt.Sprintf("Deployment mutation gate locked by %s: %s (expires in %.0fs)", s.MutationGate.Owner, s.MutationGate.Reason, s.MutationGate.ExpiresInSeconds)
	} else {
		gateMsg = "Mutation gate open"
	}
	alerts = append(alerts, Alert{
		Name:         AlertMutationGateLocked,
		Level:        gateLevel,
		Active:       gateLocked,
		Message:      gateMsg,
		Threshold:    "mutation gate locked during deployment",
		CurrentValue: s.MutationGate.GateState,
	})

	// Runtime role drift
	roleMismatch := false
	var roleLevel AlertLevel = AlertInfo
	var roleMsg string
	if s.Deployment.RuntimeRole != "" &&
		s.Deployment.RuntimeRole != "gateway" &&
		s.Deployment.RuntimeRole != "worker" &&
		s.Deployment.RuntimeRole != "monolith-dev" {
		roleMismatch = true
		roleLevel = AlertCritical
		roleMsg = fmt.Sprintf("Unknown runtime role configured: %s", s.Deployment.RuntimeRole)
	} else {
		roleMsg = "Runtime role is recognized"
	}
	alerts = append(alerts, Alert{
		Name:         AlertWrongRoleReleaseSchema,
		Level:        roleLevel,
		Active:       roleMismatch,
		Message:      roleMsg,
		Threshold:    "RUNTIME_ROLE must be gateway, worker, or monolith-dev",
		CurrentValue: s.Deployment.RuntimeRole,
	})

	streamActive := false
	streamLevel := AlertInfo
	streamMsg := "Realtime stream disabled"
	if s.Realtime.StreamEnabled {
		streamMsg = "Realtime stream connected"
		if s.Realtime.StreamState != "connected" {
			age := time.Duration(s.Realtime.StreamDisconnectedAgeSeconds * float64(time.Second))
			streamMsg = fmt.Sprintf("Realtime stream %s: %s", s.Realtime.StreamState, s.Realtime.StreamReason)
			if s.Realtime.StreamState == "stopped" {
				streamActive = true
				streamLevel = AlertCritical
			} else if age > 30*time.Second {
				streamActive = true
				streamLevel = AlertWarning
				if age > 120*time.Second {
					streamLevel = AlertCritical
				}
			}
		}
	}
	alerts = append(alerts, Alert{
		Name:         AlertRealtimeStreamDegraded,
		Level:        streamLevel,
		Active:       streamActive,
		Message:      streamMsg,
		Threshold:    "enabled stream disconnected > 30s warning, > 120s or stopped critical",
		CurrentValue: s.Realtime.StreamState,
	})

	fallbackActive := s.Realtime.StreamEnabled && s.Realtime.RecentFallbackRecoveryReconciles >= 2
	fallbackLevel := AlertInfo
	fallbackMsg := "Realtime fallback recovery within normal range"
	if fallbackActive {
		fallbackLevel = AlertWarning
		fallbackMsg = fmt.Sprintf("Realtime fallback recovered events in %d reconciles during the last minute", s.Realtime.RecentFallbackRecoveryReconciles)
	}
	alerts = append(alerts, Alert{
		Name:         AlertRealtimeFallbackRecovery,
		Level:        fallbackLevel,
		Active:       fallbackActive,
		Message:      fallbackMsg,
		Threshold:    "at least 2 recovery reconciles in 60 seconds",
		CurrentValue: s.Realtime.RecentFallbackRecoveryReconciles,
	})

	return alerts
}
