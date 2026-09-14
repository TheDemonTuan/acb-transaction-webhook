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
	AlertStaleRealtimePoll        = "ALERT_STALE_REALTIME_POLL"
	AlertStaleWorker              = "ALERT_STALE_WORKER"
	AlertAuthStuck                = "ALERT_AUTH_STUCK"
	AlertQueueSaturation          = "ALERT_QUEUE_SATURATION"
	AlertHistoryStall             = "ALERT_HISTORY_STALL"
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

	// 1. Stale Realtime Poll (>120s WARNING, >300s CRITICAL)
	stalePoll := false
	var pollLevel AlertLevel = AlertInfo
	var pollMsg string
	if s.Realtime.LastACBPollAt != "" && s.Realtime.LastACBPollAgeSeconds > 120 {
		stalePoll = true
		if s.Realtime.LastACBPollAgeSeconds > 300 {
			pollLevel = AlertCritical
			pollMsg = fmt.Sprintf("Realtime poll is critical: %.0fs since last poll", s.Realtime.LastACBPollAgeSeconds)
		} else {
			pollLevel = AlertWarning
			pollMsg = fmt.Sprintf("Realtime poll is delayed: %.0fs since last poll", s.Realtime.LastACBPollAgeSeconds)
		}
	} else {
		pollMsg = "Realtime poll age within SLA"
	}
	alerts = append(alerts, Alert{
		Name:         AlertStaleRealtimePoll,
		Level:        pollLevel,
		Active:       stalePoll,
		Message:      pollMsg,
		Threshold:    "> 120s warning, > 300s critical",
		CurrentValue: s.Realtime.LastACBPollAgeSeconds,
	})

	// 2. Stale Worker / Worker Restart Exhaustion (>60s heartbeat or IsStale true)
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

	// 3. Auth Stuck / Repeated Auth Required (>10m active attempt or session AUTH_REQUIRED)
	authStuck := false
	var authLevel AlertLevel = AlertInfo
	var authMsg string
	if s.AuthLifecycle.ActiveAttemptStuck || s.AuthLifecycle.ActiveAttemptAgeSeconds > 600 {
		authStuck = true
		authLevel = AlertWarning
		authMsg = fmt.Sprintf("Active auth attempt stuck for %.0fs without completion", s.AuthLifecycle.ActiveAttemptAgeSeconds)
	} else if s.AuthLifecycle.SessionState == "AUTH_REQUIRED" {
		authStuck = true
		authLevel = AlertWarning
		authMsg = "ACB session requires user authentication"
	} else {
		authMsg = "Auth lifecycle normal"
	}
	alerts = append(alerts, Alert{
		Name:         AlertAuthStuck,
		Level:        authLevel,
		Active:       authStuck,
		Message:      authMsg,
		Threshold:    "> 600s active attempt or session=AUTH_REQUIRED",
		CurrentValue: s.AuthLifecycle.ActiveAttemptAgeSeconds,
	})

	// 4. Queue Saturation (>20 queue depth, >300s oldest queued job, or overload events)
	queueSaturated := false
	var queueLevel AlertLevel = AlertInfo
	var queueMsg string
	if s.Scheduler.TotalQueueDepth > 20 || s.Scheduler.Overloaded > 0 || s.HistoryJobs.OldestQueuedAgeSeconds > 300 {
		queueSaturated = true
		if s.Scheduler.TotalQueueDepth > 50 || s.Scheduler.Overloaded > 5 {
			queueLevel = AlertCritical
			queueMsg = fmt.Sprintf("Scheduler queue saturated: depth=%d, overloaded=%d, oldestQueued=%.0fs", s.Scheduler.TotalQueueDepth, s.Scheduler.Overloaded, s.HistoryJobs.OldestQueuedAgeSeconds)
		} else {
			queueLevel = AlertWarning
			queueMsg = fmt.Sprintf("Scheduler backlog elevated: depth=%d, oldestQueued=%.0fs", s.Scheduler.TotalQueueDepth, s.HistoryJobs.OldestQueuedAgeSeconds)
		}
	} else {
		queueMsg = "Scheduler queues within normal operating limits"
	}
	alerts = append(alerts, Alert{
		Name:         AlertQueueSaturation,
		Level:        queueLevel,
		Active:       queueSaturated,
		Message:      queueMsg,
		Threshold:    "depth > 20, oldestQueued > 300s, or overload events",
		CurrentValue: s.Scheduler.TotalQueueDepth,
	})

	// 5. History Job Stall (>0 stalled jobs past heartbeat deadline)
	historyStall := false
	var histLevel AlertLevel = AlertInfo
	var histMsg string
	if s.HistoryJobs.StalledCount > 0 {
		historyStall = true
		histLevel = AlertWarning
		histMsg = fmt.Sprintf("%d running history job(s) stalled without recent heartbeat", s.HistoryJobs.StalledCount)
	} else {
		histMsg = "No stalled history jobs"
	}
	alerts = append(alerts, Alert{
		Name:         AlertHistoryStall,
		Level:        histLevel,
		Active:       historyStall,
		Message:      histMsg,
		Threshold:    "stalled jobs > 0 (> 300s without heartbeat)",
		CurrentValue: s.HistoryJobs.StalledCount,
	})

	// 6. Notification Backlog Stuck (dead letter > 10 or pending > 50)
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

	// 7. Backup Overdue (>86400s / 24 hours)
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

	// 8. Restore Drill Overdue (>30 days or drill failed)
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

	// 9. Mutation Gate Locked (>900s / 15m)
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

	// 10. Wrong Role / Release / Schema
	roleMismatch := false
	var roleLevel AlertLevel = AlertInfo
	var roleMsg string
	if s.Deployment.RuntimeRole != "" &&
		s.Deployment.RuntimeRole != "gateway" &&
		s.Deployment.RuntimeRole != "worker" &&
		s.Deployment.RuntimeRole != "all-in-one" {
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
		Threshold:    "RUNTIME_ROLE must be gateway, worker, or all-in-one",
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
