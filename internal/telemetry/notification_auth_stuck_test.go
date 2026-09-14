package telemetry

import (
	"testing"
	"time"
)

func TestTelemetry_NotificationAndAuthStuckAlerts(t *testing.T) {
	reg := NewRegistry()

	t.Run("normal notification backlog produces no alert", func(t *testing.T) {
		reg.SetNotificationBacklog(3, 0, false, map[string]ProviderSnapshot{
			"WEBHOOK": {Pending: 2, DeadLetter: 0, Success: 100},
			"BARK":    {Pending: 1, DeadLetter: 0, Success: 50},
		})

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var notifAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertNotificationBacklogStuck {
				notifAlert = &a
				break
			}
		}
		if notifAlert == nil || notifAlert.Active {
			t.Errorf("expected notification alert inactive, got: %v", notifAlert)
		}
	})

	t.Run("notification dead letters > 10 triggers ALERT_NOTIFICATION_BACKLOG_STUCK", func(t *testing.T) {
		reg.SetNotificationBacklog(5, 12, true, map[string]ProviderSnapshot{
			"WEBHOOK": {Pending: 5, DeadLetter: 12},
		})

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var notifAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertNotificationBacklogStuck {
				notifAlert = &a
				break
			}
		}
		if notifAlert == nil || !notifAlert.Active {
			t.Fatalf("expected ALERT_NOTIFICATION_BACKLOG_STUCK active for 12 dead letters")
		}
		if notifAlert.Level != AlertWarning {
			t.Errorf("expected WARNING level, got %v", notifAlert.Level)
		}
	})

	t.Run("normal auth lifecycle produces no alert", func(t *testing.T) {
		reg.SetAuthLifecycle(true, 45*time.Second, false, "READY", 5, map[string]int{"ACTIVE": 1, "COMPLETED": 4})

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var authAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertAuthStuck {
				authAlert = &a
				break
			}
		}
		if authAlert == nil || authAlert.Active {
			t.Errorf("expected auth alert inactive for 45s attempt, got: %v", authAlert)
		}
	})

	t.Run("active auth attempt older than 10 minutes triggers ALERT_AUTH_STUCK", func(t *testing.T) {
		reg.SetAuthLifecycle(true, 650*time.Second, true, "ACTIVE", 6, map[string]int{"ACTIVE": 1})

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var authAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertAuthStuck {
				authAlert = &a
				break
			}
		}
		if authAlert == nil || !authAlert.Active {
			t.Fatalf("expected ALERT_AUTH_STUCK active for 650s attempt")
		}
		if authAlert.Level != AlertWarning {
			t.Errorf("expected WARNING level, got %v", authAlert.Level)
		}
	})

	t.Run("session state AUTH_REQUIRED triggers ALERT_AUTH_STUCK", func(t *testing.T) {
		reg.SetAuthLifecycle(false, 0, false, "AUTH_REQUIRED", 1, map[string]int{})

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var authAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertAuthStuck {
				authAlert = &a
				break
			}
		}
		if authAlert == nil || !authAlert.Active {
			t.Fatalf("expected ALERT_AUTH_STUCK active for AUTH_REQUIRED session")
		}
	})

	t.Run("stalled history jobs triggers ALERT_HISTORY_STALL", func(t *testing.T) {
		reg.SetHistoryJobs(map[string]int{"RUNNING": 2}, 0, 2, 10, 200) // 2 stalled

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var histAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertHistoryStall {
				histAlert = &a
				break
			}
		}
		if histAlert == nil || !histAlert.Active {
			t.Fatalf("expected ALERT_HISTORY_STALL active for 2 stalled jobs")
		}
	})
}
