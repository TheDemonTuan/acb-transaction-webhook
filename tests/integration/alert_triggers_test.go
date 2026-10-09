package integration_test

import (
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

// TestAlertThresholdTriggers covers the operational alerts retained by the payOS runtime.
// An idle payment channel is healthy: lack of incoming webhooks is not a stale poll.
func TestAlertThresholdTriggers(t *testing.T) {
	now := time.Now().UTC()
	normal := telemetry.TelemetrySnapshot{
		CapturedAt: now,
		Realtime: telemetry.RealtimeTelemetry{
			StreamEnabled: true,
			StreamState:   "connected",
		},
		Singleton: telemetry.SingletonTelemetry{
			WorkerState:         "READY",
			HeartbeatAt:         now.Format(time.RFC3339),
			HeartbeatAgeSeconds: 30,
		},
		Deployment: telemetry.DeploymentTelemetry{RuntimeRole: "gateway"},
		BackupAndRestore: telemetry.BackupAndDrillTelemetry{
			LastBackupAt:            now.Format(time.RFC3339),
			LastRestoreDrillAt:      now.Format(time.RFC3339),
			LastRestoreDrillSuccess: true,
		},
	}
	for _, alert := range telemetry.EvaluateAlerts(normal) {
		if alert.Active {
			t.Fatalf("unexpected active alert on idle healthy channel: %+v", alert)
		}
	}

	for _, test := range []struct {
		name   string
		alert  string
		level  telemetry.AlertLevel
		mutate func(*telemetry.TelemetrySnapshot)
	}{
		{"worker heartbeat stale", telemetry.AlertStaleWorker, telemetry.AlertCritical, func(s *telemetry.TelemetrySnapshot) {
			s.Singleton.HeartbeatAgeSeconds = 61
		}},
		{"worker unhealthy", telemetry.AlertStaleWorker, telemetry.AlertWarning, func(s *telemetry.TelemetrySnapshot) {
			s.Singleton.WorkerState = "FAILED"
		}},
		{"pending notification backlog", telemetry.AlertNotificationBacklogStuck, telemetry.AlertWarning, func(s *telemetry.TelemetrySnapshot) {
			s.Notifications.TotalPending = 51
		}},
		{"notification dead letters", telemetry.AlertNotificationBacklogStuck, telemetry.AlertWarning, func(s *telemetry.TelemetrySnapshot) {
			s.Notifications.TotalDeadLetter = 11
		}},
		{"backup overdue", telemetry.AlertBackupOverdue, telemetry.AlertWarning, func(s *telemetry.TelemetrySnapshot) {
			s.BackupAndRestore.BackupAgeSeconds = 86401
		}},
		{"restore drill overdue", telemetry.AlertRestoreDrillOverdue, telemetry.AlertWarning, func(s *telemetry.TelemetrySnapshot) {
			s.BackupAndRestore.RestoreDrillAgeDays = 31
		}},
		{"restore drill failed", telemetry.AlertRestoreDrillOverdue, telemetry.AlertCritical, func(s *telemetry.TelemetrySnapshot) {
			s.BackupAndRestore.LastRestoreDrillSuccess = false
		}},
		{"deployment gate locked", telemetry.AlertMutationGateLocked, telemetry.AlertWarning, func(s *telemetry.TelemetrySnapshot) {
			s.MutationGate.IsLocked = true
			s.MutationGate.GateState = "LOCKED"
		}},
		{"runtime role drift", telemetry.AlertWrongRoleReleaseSchema, telemetry.AlertCritical, func(s *telemetry.TelemetrySnapshot) {
			s.Deployment.RuntimeRole = "unexpected-role"
		}},
		{"realtime disconnected warning", telemetry.AlertRealtimeStreamDegraded, telemetry.AlertWarning, func(s *telemetry.TelemetrySnapshot) {
			s.Realtime.StreamState = "reconnecting"
			s.Realtime.StreamDisconnectedAgeSeconds = 31
		}},
		{"realtime disconnected critical", telemetry.AlertRealtimeStreamDegraded, telemetry.AlertCritical, func(s *telemetry.TelemetrySnapshot) {
			s.Realtime.StreamState = "reconnecting"
			s.Realtime.StreamDisconnectedAgeSeconds = 121
		}},
		{"realtime stopped", telemetry.AlertRealtimeStreamDegraded, telemetry.AlertCritical, func(s *telemetry.TelemetrySnapshot) {
			s.Realtime.StreamState = "stopped"
		}},
		{"journal fallback elevated", telemetry.AlertRealtimeFallbackRecovery, telemetry.AlertWarning, func(s *telemetry.TelemetrySnapshot) {
			s.Realtime.RecentFallbackRecoveryReconciles = 2
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := normal
			test.mutate(&snapshot)
			for _, alert := range telemetry.EvaluateAlerts(snapshot) {
				if alert.Name == test.alert {
					if !alert.Active || alert.Level != test.level {
						t.Fatalf("expected active %s alert, got %+v", test.level, alert)
					}
					return
				}
			}
			t.Fatalf("expected alert %s not found", test.alert)
		})
	}

	for _, role := range []string{"gateway", "worker", "monolith-dev"} {
		t.Run("recognized role "+role, func(t *testing.T) {
			snapshot := normal
			snapshot.Deployment.RuntimeRole = role
			for _, alert := range telemetry.EvaluateAlerts(snapshot) {
				if alert.Name == telemetry.AlertWrongRoleReleaseSchema && alert.Active {
					t.Fatalf("supported runtime role produced drift alert: %+v", alert)
				}
			}
		})
	}
}
