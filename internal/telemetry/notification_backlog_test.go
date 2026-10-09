package telemetry

import "testing"

func TestTelemetry_NotificationBacklogAlerts(t *testing.T) {
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

}
