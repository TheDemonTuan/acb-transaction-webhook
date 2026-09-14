package telemetry

import (
	"testing"
	"time"
)

func TestTelemetry_QueueSaturationAlert(t *testing.T) {
	reg := NewRegistry()

	t.Run("normal queue depth produces no saturation alert", func(t *testing.T) {
		reg.SetSchedulerQueue(map[string]int{
			"REALTIME_POLL":  1,
			"FILTER_HISTORY": 3,
		}, 4)
		reg.SetHistoryJobs(map[string]int{"QUEUED": 2, "RUNNING": 1}, 10*time.Second, 0, 5, 100)

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var satAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertQueueSaturation {
				satAlert = &a
				break
			}
		}
		if satAlert == nil {
			t.Fatal("ALERT_QUEUE_SATURATION not found in alerts")
		}
		if satAlert.Active {
			t.Errorf("expected queue saturation alert to be inactive, got active with message: %s", satAlert.Message)
		}
	})

	t.Run("queue depth exceeding threshold triggers WARNING alert", func(t *testing.T) {
		reg.SetSchedulerQueue(map[string]int{
			"REALTIME_POLL":  2,
			"FILTER_HISTORY": 20,
			"CATCH_UP":       3,
		}, 25) // Total > 20

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var satAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertQueueSaturation {
				satAlert = &a
				break
			}
		}
		if satAlert == nil || !satAlert.Active {
			t.Fatalf("expected queue saturation alert to be active for depth 25")
		}
		if satAlert.Level != AlertWarning {
			t.Errorf("expected WARNING level for depth 25, got %v", satAlert.Level)
		}
	})

	t.Run("severe queue depth triggers CRITICAL alert", func(t *testing.T) {
		reg.SetSchedulerQueue(map[string]int{
			"FILTER_HISTORY": 55,
		}, 55) // Total > 50

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var satAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertQueueSaturation {
				satAlert = &a
				break
			}
		}
		if satAlert == nil || !satAlert.Active {
			t.Fatalf("expected queue saturation alert to be active for depth 55")
		}
		if satAlert.Level != AlertCritical {
			t.Errorf("expected CRITICAL level for depth 55, got %v", satAlert.Level)
		}
	})

	t.Run("oldest queued age exceeding 300s triggers alert", func(t *testing.T) {
		reg.SetSchedulerQueue(map[string]int{"REALTIME_POLL": 1}, 1)
		reg.SetHistoryJobs(map[string]int{"QUEUED": 1}, 350*time.Second, 0, 0, 0)

		snap := reg.FullSnapshot()
		alerts := EvaluateAlerts(snap)

		var satAlert *Alert
		for _, a := range alerts {
			if a.Name == AlertQueueSaturation {
				satAlert = &a
				break
			}
		}
		if satAlert == nil || !satAlert.Active {
			t.Fatalf("expected queue saturation alert for oldestQueuedAge 350s")
		}
	})
}
