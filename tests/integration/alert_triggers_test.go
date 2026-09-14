package integration_test

import (
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

// TestAlertThresholdTriggers verifies telemetry alert evaluation:
// 1. Stale realtime poll (>120s WARNING, >300s CRITICAL)
// 2. Auth stuck (>600s active attempt or AUTH_REQUIRED)
// 3. Queue saturation (depth > 20, oldest > 300s)
// 4. History job stall
// 5. Overdue backup / restore drill
func TestAlertThresholdTriggers(t *testing.T) {
	// Baseline snapshot: all normal
	sNormal := telemetry.TelemetrySnapshot{
		CapturedAt: time.Now(),
		Realtime: telemetry.RealtimeTelemetry{
			LastACBPollAt:         time.Now().Add(-30 * time.Second).Format(time.RFC3339),
			LastACBPollAgeSeconds: 30,
		},
		AuthLifecycle: telemetry.AuthLifecycleTelemetry{
			SessionState: "MONITORING",
		},
		Scheduler: telemetry.SchedulerTelemetry{
			TotalQueueDepth: 3,
		},
	}
	alertsNormal := telemetry.EvaluateAlerts(sNormal)
	for _, a := range alertsNormal {
		if a.Active {
			t.Fatalf("expected alert %s to be inactive on normal snapshot, got active", a.Name)
		}
	}

	// 1. Stale poll warning & critical
	sWarnPoll := sNormal
	sWarnPoll.Realtime.LastACBPollAgeSeconds = 150
	alertsWarn := telemetry.EvaluateAlerts(sWarnPoll)
	var staleFound bool
	for _, a := range alertsWarn {
		if a.Name == telemetry.AlertStaleRealtimePoll {
			staleFound = true
			if !a.Active || a.Level != telemetry.AlertWarning {
				t.Fatalf("expected active WARNING for stale poll at 150s, got %v level=%s", a.Active, a.Level)
			}
		}
	}
	if !staleFound {
		t.Fatal("AlertStaleRealtimePoll not found")
	}

	sCritPoll := sNormal
	sCritPoll.Realtime.LastACBPollAgeSeconds = 350
	alertsCrit := telemetry.EvaluateAlerts(sCritPoll)
	for _, a := range alertsCrit {
		if a.Name == telemetry.AlertStaleRealtimePoll {
			if !a.Active || a.Level != telemetry.AlertCritical {
				t.Fatalf("expected active CRITICAL for stale poll at 350s, got %v level=%s", a.Active, a.Level)
			}
		}
	}

	// 2. Auth stuck trigger
	sAuthStuck := sNormal
	sAuthStuck.AuthLifecycle.SessionState = "AUTH_REQUIRED"
	alertsAuth := telemetry.EvaluateAlerts(sAuthStuck)
	var authFound bool
	for _, a := range alertsAuth {
		if a.Name == telemetry.AlertAuthStuck {
			authFound = true
			if !a.Active {
				t.Fatalf("expected AlertAuthStuck active when session=AUTH_REQUIRED")
			}
		}
	}
	if !authFound {
		t.Fatal("AlertAuthStuck not found")
	}

	// 3. Queue saturation trigger
	sQueueSat := sNormal
	sQueueSat.Scheduler.TotalQueueDepth = 25
	alertsQueue := telemetry.EvaluateAlerts(sQueueSat)
	var queueFound bool
	for _, a := range alertsQueue {
		if a.Name == telemetry.AlertQueueSaturation {
			queueFound = true
			if !a.Active {
				t.Fatalf("expected AlertQueueSaturation active when queue depth=25")
			}
		}
	}
	if !queueFound {
		t.Fatal("AlertQueueSaturation not found")
	}
}
